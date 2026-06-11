package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent/codex"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitdiff"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/plan/planfixture"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// openBundleForTest is a thin wrapper around bundle.Open so the
// table-driven test reads cleanly. We round-trip in tests because
// the runner is the producer; the canonical verifier path lives in
// /verifier (and it intentionally re-implements bundle parsing
// rather than importing this code, per ADR-008's no-trust model).
// gunzip is a small test helper for inspecting a runner-produced
// bundle. The runner's wire format is gzipped JSONL; redaction
// assertions need to look at the decompressed text.
func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func openBundleForTest(data []byte) (bundle.ManifestData, []bundle.Line, bundle.TrailerData, error) {
	return bundle.Open(bytes.NewReader(data))
}

// fakeInvoker lets tests drive run() without spawning a child
// process. Returning (canned, returnErr) keeps the seam tiny.
//
// For sequence tests (e.g. self-retry): set cannedSeq + errSeq instead
// of canned + returnErr. Each Invoke call consumes the next entry; the
// last entry repeats after the slice is exhausted.
type fakeInvoker struct {
	canned    agent.Result
	returnErr error
	gotAPIKey string
	// gotInv captures the Invocation the harness handed in, so
	// tests can assert on plumbed Env (E19.8 / #348 wiring).
	gotInv *agent.Invocation

	// cannedSeq / errSeq support tests that need different results per
	// call. When cannedSeq is non-nil it takes precedence over canned.
	cannedSeq []agent.Result
	errSeq    []error
	callIdx   int

	// onInvoke, when set, runs before each canned result is returned,
	// receiving the 0-based call index and the Invocation. The verify-fix
	// loop tests (#651) use it to mutate the working tree between invocations
	// so the committed scope-only tree changes across iterations.
	onInvoke func(callIdx int, inv agent.Invocation)
}

func (f *fakeInvoker) Invoke(_ context.Context, inv agent.Invocation) (agent.Result, error) {
	i := inv
	f.gotInv = &i
	idx := f.callIdx
	f.callIdx++
	if f.onInvoke != nil {
		f.onInvoke(idx, inv)
	}
	if len(f.cannedSeq) > 0 {
		si := idx
		if si >= len(f.cannedSeq) {
			si = len(f.cannedSeq) - 1
		}
		var err error
		if si < len(f.errSeq) {
			err = f.errSeq[si]
		}
		return f.cannedSeq[si], err
	}
	return f.canned, f.returnErr
}

// withFakeInvoker swaps the package's newInvoker for one that
// records the API key and returns canned results. Cleanup restores
// the original constructor.
func withFakeInvoker(t *testing.T, fake *fakeInvoker) {
	t.Helper()
	orig := newInvoker
	newInvoker = func(apiKey string) agent.Invoker {
		fake.gotAPIKey = apiKey
		return fake
	}
	t.Cleanup(func() { newInvoker = orig })
}

// fakeUploader stands in for upload.Client during run() tests.
// Captures the IssueKey + ShipTrace + FetchPrompt inputs so
// assertions can confirm the runner wired the right
// run/stage/variant/bundle.
type fakeUploader struct {
	issueErr      error
	shipErr       error
	promptErr     error
	planErr       error
	prErr         error
	instTokenErr  error
	mcpTokenErr   error
	retryStageErr error

	// Recorded calls.
	gotIssueRunID string
	gotIssueCount int
	// gotShipArgs points at the first ShipTrace call so existing
	// tests that assert on it without caring about variant keep
	// working — for runs that ship both raw + redacted (the v0
	// default after E2.4) the first call is the raw upload.
	gotShipArgs *upload.ShipArgs
	// gotShipCalls captures every ShipTrace call in order so tests
	// can assert that both variants ship per stage.
	gotShipCalls     []upload.ShipArgs
	gotPromptArgs    *upload.FetchPromptArgs
	gotPlanArgs      *upload.ShipPlanArgs
	gotPRArgs        *upload.ShipPullRequestArgs
	gotInstTokenArgs *upload.FetchInstallationTokenArgs
	gotMCPTokenArgs  *upload.FetchMCPTokenArgs
	gotRetryArgs     []upload.RetryStageArgs

	// Scope-amendment refresh seam (#961): amendments is what
	// FetchScopeAmendments returns; gotAmendmentArgs records the call
	// so tests can assert the run-bound bearer is reused.
	amendments       []upload.ScopeAmendment
	amendmentsErr    error
	gotAmendmentArgs *upload.FetchScopeAmendmentsArgs

	// Canned prompt response. If nil, FetchPrompt returns a default
	// one matching the requested stage_id.
	promptResp *upload.FetchedPrompt

	// Generated by IssueKey when issueErr is nil; ShipTrace returns
	// success keyed off the same key so a real signature is
	// unnecessary in tests.
	priv ed25519.PrivateKey
}

func newFakeUploader(t *testing.T) *fakeUploader {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeUploader{priv: priv}
}

func (f *fakeUploader) IssueKey(_ context.Context, runID string, _ time.Duration) (*upload.IssuedKey, error) {
	f.gotIssueRunID = runID
	f.gotIssueCount++
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return &upload.IssuedKey{
		RunID:      runID,
		PrivateKey: f.priv,
		PublicKey:  f.priv.Public().(ed25519.PublicKey),
		IssuedAt:   time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 5, 2, 12, 30, 0, 0, time.UTC),
	}, nil
}

func (f *fakeUploader) FetchPrompt(_ context.Context, args upload.FetchPromptArgs) (*upload.FetchedPrompt, error) {
	a := args
	f.gotPromptArgs = &a
	if f.promptErr != nil {
		return nil, f.promptErr
	}
	if f.promptResp != nil {
		r := *f.promptResp
		return &r, nil
	}
	return &upload.FetchedPrompt{
		StageID:   args.StageID,
		StageType: "plan", // default to a non-implement stage so the
		// PR-upload branch in run() doesn't fire on every test that
		// touches FetchPrompt. Implement-specific tests override via
		// fu.promptResp.StageType = "implement".
		Prompt:     "fake prompt body for stage " + args.StageID,
		PromptHash: "deadbeef",
	}, nil
}

func (f *fakeUploader) ShipTrace(_ context.Context, args upload.ShipArgs) (*upload.ShipResult, error) {
	a := args
	f.gotShipCalls = append(f.gotShipCalls, a)
	if f.gotShipArgs == nil {
		// Hold onto the first call so legacy tests stay happy; the
		// gotShipCalls slice is the canonical record of every call.
		f.gotShipArgs = &a
	}
	if f.shipErr != nil {
		return nil, f.shipErr
	}
	return &upload.ShipResult{
		RunID:       args.RunID,
		StageID:     args.StageID,
		Variant:     args.Variant,
		ContentHash: "deadbeef",
	}, nil
}

func (f *fakeUploader) ShipPlan(_ context.Context, args upload.ShipPlanArgs) (*upload.ShipPlanResult, error) {
	a := args
	f.gotPlanArgs = &a
	if f.planErr != nil {
		return nil, f.planErr
	}
	return &upload.ShipPlanResult{
		ID:            "00000000-0000-0000-0000-000000000aaa",
		StageID:       args.StageID,
		ContentHash:   "deadbeef",
		SchemaVersion: "standard_v1",
	}, nil
}

func (f *fakeUploader) ShipPullRequest(_ context.Context, args upload.ShipPullRequestArgs) (*upload.ShipPullRequestResult, error) {
	a := args
	f.gotPRArgs = &a
	if f.prErr != nil {
		return nil, f.prErr
	}
	return &upload.ShipPullRequestResult{
		ID:          "00000000-0000-0000-0000-000000000bbb",
		StageID:     args.StageID,
		ContentHash: "cafebabe",
		PRNumber:    42,
		PRURL:       "https://github.com/x/y/pull/42",
		HeadSHA:     "abc",
	}, nil
}

func (f *fakeUploader) FetchInstallationToken(_ context.Context, args upload.FetchInstallationTokenArgs) (*upload.FetchInstallationTokenResult, error) {
	a := args
	f.gotInstTokenArgs = &a
	if f.instTokenErr != nil {
		return nil, f.instTokenErr
	}
	return &upload.FetchInstallationTokenResult{Token: "ghs_app_token"}, nil
}

// FetchMCPToken stubs the E19.8 / #348 endpoint. The default
// happy-path response is sufficient for tests that just need the
// agent invocation to proceed; the per-test fields below let
// individual tests assert on arguments or inject errors.
func (f *fakeUploader) FetchMCPToken(_ context.Context, args upload.FetchMCPTokenArgs) (*upload.FetchMCPTokenResult, error) {
	a := args
	f.gotMCPTokenArgs = &a
	if f.mcpTokenErr != nil {
		return nil, f.mcpTokenErr
	}
	return &upload.FetchMCPTokenResult{
		Token:     "fhm_stubmcptokenforuse",
		TokenID:   "t-id-stub",
		RunID:     args.RunID,
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

// FetchScopeAmendments stubs the #961 mid-stage refresh endpoint.
// Returns f.amendments verbatim; records the args so tests can assert
// the runner reused the run-bound fhm_ bearer.
func (f *fakeUploader) FetchScopeAmendments(_ context.Context, args upload.FetchScopeAmendmentsArgs) ([]upload.ScopeAmendment, error) {
	a := args
	f.gotAmendmentArgs = &a
	if f.amendmentsErr != nil {
		return nil, f.amendmentsErr
	}
	return f.amendments, nil
}

func (f *fakeUploader) RetryStage(_ context.Context, args upload.RetryStageArgs) error {
	f.gotRetryArgs = append(f.gotRetryArgs, args)
	return f.retryStageErr
}

// withFakeUploader swaps newUploadClient. Caller restores via
// t.Cleanup automatically.
func withFakeUploader(t *testing.T, fake *fakeUploader) {
	t.Helper()
	orig := newUploadClient
	newUploadClient = func(_ string) uploadClient { return fake }
	t.Cleanup(func() { newUploadClient = orig })
}

// TestRun_HappyPath exercises the no-op success path: every
// required flag set, run() returns 0 and writes a startup log line.
func TestRun_HappyPath(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &out)
	if got != exitOK {
		t.Errorf("run = %d, want %d", got, exitOK)
	}
	for _, want := range []string{
		`"event":"runner_started"`,
		`"run_id":"11111111-2222-3333-4444-555555555555"`,
		`"workflow":"feature_change"`,
		`"stage":"plan"`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("startup log missing %s:\n%s", want, out.String())
		}
	}
}

func TestRun_MissingRunID(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
	}, &out)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRun_BadFlag(t *testing.T) {
	got := run([]string{"--no-such-flag"}, io.Discard)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRun_HelpExitsUsage(t *testing.T) {
	// flag.ContinueOnError + --help surfaces ErrHelp. We treat that
	// as a usage exit, same as a malformed flag.
	got := run([]string{"--help"}, io.Discard)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
}

func TestRunnerVersion_NonEmpty(t *testing.T) {
	if runnerVersion() == "" {
		t.Fatal("runnerVersion() should never be empty")
	}
}

func TestRun_PromptFileMissing(t *testing.T) {
	var out strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", "/no/such/path/anywhere.txt",
	}, &out)
	if got != exitUsage {
		t.Errorf("run = %d, want %d", got, exitUsage)
	}
	if !strings.Contains(out.String(), `"event":"runner_failed"`) {
		t.Errorf("missing runner_failed log line: %s", out.String())
	}
	if !strings.Contains(out.String(), `"reason":"read_prompt"`) {
		t.Errorf("missing read_prompt reason: %s", out.String())
	}
}

func TestClassifyErr(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{agent.ErrTimeout, "timeout"},
		{fmt.Errorf("wrapped: %w", agent.ErrTimeout), "timeout"},
		{agent.ErrBudgetExceeded, "budget_exceeded"},
		{agent.ErrBinaryNotFound, "binary_not_found"},
		{agent.ErrAgentThinkingBlock, "agent_api_thinking_block"},
		{fmt.Errorf("wrapped: %w", agent.ErrAgentThinkingBlock), "agent_api_thinking_block"},
		{agent.ErrLoopDetected, "loop_detected"},
		{fmt.Errorf("wrapped: %w", agent.ErrLoopDetected), "loop_detected"},
		{agent.ErrAgentFailed, "agent_failed"},
		{fmt.Errorf("wrapped: %w", agent.ErrAgentFailed), "agent_failed"},
		{errors.New("anything else"), "other"},
	}
	for _, tc := range cases {
		var name string
		if tc.err == nil {
			name = "nil"
		} else {
			name = tc.err.Error()
		}
		t.Run(name, func(t *testing.T) {
			if got := classifyErr(tc.err); got != tc.want {
				t.Errorf("classifyErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestLogCompletion_OK(t *testing.T) {
	var w strings.Builder
	logCompletion(&w, agent.Result{OK: true, TokensUsed: 250}, nil)
	out := w.String()
	if !strings.Contains(out, `"outcome":"ok"`) {
		t.Errorf("missing outcome ok: %s", out)
	}
	if !strings.Contains(out, `"tokens_used":250`) {
		t.Errorf("missing tokens_used: %s", out)
	}
}

func TestLogCompletion_Failure(t *testing.T) {
	var w strings.Builder
	logCompletion(&w, agent.Result{
		OK:              false,
		FailureCategory: "A",
		FailureReason:   "agent timeout after 100ms",
		TokensUsed:      0,
	}, agent.ErrTimeout)
	out := w.String()
	for _, want := range []string{
		`"outcome":"failed"`,
		`"category":"A"`,
		`"reason":"agent timeout after 100ms"`,
		`"err_class":"timeout"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in: %s", want, out)
		}
	}
}

func TestRun_PromptInvokesAgentAndEmitsEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-test-1234")

	fake := &fakeInvoker{
		canned: agent.Result{
			OK:         true,
			TokensUsed: 250,
			Events: []agent.Event{
				{Kind: "invocation_start", Payload: agent.MakePayload(map[string]string{"a": "b"})},
				{Kind: "result", Payload: agent.MakePayload(map[string]int{"n": 1})},
				{Kind: "invocation_end"},
			},
		},
	}
	withFakeInvoker(t, fake)

	// Capture stdout — emitEvents writes there.
	stdoutR, stdoutW, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = origStdout })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", path,
		"--max-tokens", "1000",
		"--timeout", "30s",
	}, &stderr)

	_ = stdoutW.Close()
	stdoutBytes, _ := io.ReadAll(stdoutR)

	if got != exitOK {
		t.Errorf("run = %d, want %d", got, exitOK)
	}
	if fake.gotAPIKey != "sk-test-1234" {
		t.Errorf("invoker gotAPIKey = %q, want sk-test-1234", fake.gotAPIKey)
	}

	// Three events should have been emitted as JSON Lines on stdout.
	lines := bytes.Split(bytes.TrimRight(stdoutBytes, "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("emitted %d JSONL lines, want 3:\n%s", len(lines), stdoutBytes)
	}
	for i, line := range lines {
		var ev agent.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Errorf("line %d not JSON: %v: %s", i, err, line)
		}
	}

	// The completion log line should report ok and the token count.
	if !strings.Contains(stderr.String(), `"outcome":"ok"`) {
		t.Errorf("missing ok outcome in stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"tokens_used":250`) {
		t.Errorf("missing tokens_used in stderr: %s", stderr.String())
	}
}

func TestRun_AgentFailureMapsToExit1(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")

	fake := &fakeInvoker{
		canned: agent.Result{
			OK:              false,
			FailureCategory: "A",
			FailureReason:   "agent timeout after 30s",
			Events: []agent.Event{
				{Kind: "invocation_start"},
				{Kind: "invocation_end"},
			},
		},
		returnErr: agent.ErrTimeout,
	}
	withFakeInvoker(t, fake)

	// Discard stdout to keep test output clean.
	stdoutR, stdoutW, _ := os.Pipe()
	origStdout := os.Stdout
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = origStdout })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", path,
	}, &stderr)

	_ = stdoutW.Close()
	_, _ = io.ReadAll(stdoutR)

	if got != exitFailure {
		t.Errorf("run = %d, want %d", got, exitFailure)
	}
	out := stderr.String()
	if !strings.Contains(out, `"outcome":"failed"`) {
		t.Errorf("missing failed outcome: %s", out)
	}
	if !strings.Contains(out, `"category":"A"`) {
		t.Errorf("missing category A: %s", out)
	}
	if !strings.Contains(out, `"err_class":"timeout"`) {
		t.Errorf("missing err_class timeout: %s", out)
	}
}

func TestRun_PromptWithBundleOut_WritesGzipFile(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &fakeInvoker{
		canned: agent.Result{
			OK:         true,
			TokensUsed: 7,
			Events: []agent.Event{
				{Kind: "system.init", Payload: agent.MakePayload(map[string]string{"a": "b"})},
				{Kind: "result", Payload: agent.MakePayload(map[string]int{"n": 1})},
			},
		},
	}
	withFakeInvoker(t, fake)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", promptPath,
		"--bundle-out", bundlePath,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want %d:\n%s", got, exitOK, stderr.String())
	}
	info, err := os.Stat(bundlePath)
	if err != nil {
		t.Fatalf("bundle not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("bundle is empty")
	}

	// Sanity-check that the file is a valid bundle. We import
	// internal/bundle here just to round-trip-verify; in
	// production the backend re-implements verification under
	// the audit-grade no-trust constraint (see verifier/).
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, events, trailer, err := openBundleForTest(data)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if manifest.RunID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("manifest RunID = %q", manifest.RunID)
	}
	if len(events) != 2 {
		t.Errorf("events = %d, want 2", len(events))
	}
	if trailer.EventCount != 2 {
		t.Errorf("trailer EventCount = %d, want 2", trailer.EventCount)
	}
}

// TestRun_AgentFlagStampsBundleManifest crosses the flag-parse ->
// selectInvoker -> bundle-manifest stamp seam (#839): it drives run()
// with --agent (and the no-flag default) and asserts the produced
// bundle manifest stamps the selected id. The no-flag case proves the
// claude-code default is preserved end-to-end.
func TestRun_AgentFlagStampsBundleManifest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agentArgs []string
		wantAgent string
	}{
		{name: "default", agentArgs: nil, wantAgent: "claude-code"},
		{name: "explicit-claude", agentArgs: []string{"--agent", "claude-code"}, wantAgent: "claude-code"},
		{name: "explicit-codex", agentArgs: []string{"--agent", "codex"}, wantAgent: "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			promptPath := filepath.Join(dir, "prompt.txt")
			bundlePath := filepath.Join(dir, "trace.jsonl.gz")
			if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
				t.Fatal(err)
			}

			// Fake both provider seams so codex routing doesn't hit the
			// not-implemented placeholder — we only care that the selected
			// id reaches the manifest, not the agent's behavior.
			fake := &fakeInvoker{canned: agent.Result{OK: true}}
			withFakeInvoker(t, fake)
			origCodex := newCodexInvoker
			newCodexInvoker = func(string) agent.Invoker { return fake }
			t.Cleanup(func() { newCodexInvoker = origCodex })

			args := []string{
				"--run-id", "11111111-2222-3333-4444-555555555555",
				"--backend-url", "https://api.fishhawk.test",
				"--workflow", "feature_change",
				"--stage", "plan",
				"--prompt-file", promptPath,
				"--bundle-out", bundlePath,
			}
			args = append(args, tc.agentArgs...)

			var stderr strings.Builder
			if got := run(args, &stderr); got != exitOK {
				t.Fatalf("run = %d, want %d:\n%s", got, exitOK, stderr.String())
			}
			data, err := os.ReadFile(bundlePath)
			if err != nil {
				t.Fatal(err)
			}
			manifest, _, _, err := openBundleForTest(data)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if manifest.Agent != tc.wantAgent {
				t.Errorf("manifest.Agent = %q, want %q", manifest.Agent, tc.wantAgent)
			}
		})
	}
}

// TestHelperProcessCodex pretends to be the `codex` binary for the
// cross-boundary test below: it echoes the OPENAI_API_KEY it received in
// its env as a codex JSONL event, then a clean turn.completed. Gated on
// GO_HELPER_PROCESS_CODEX so it is a no-op under a normal test run.
func TestHelperProcessCodex(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS_CODEX") != "1" {
		return
	}
	defer os.Exit(0)
	fmt.Printf(`{"type":"agent_env","key":"OPENAI_API_KEY","value":%q}`+"\n",
		os.Getenv("OPENAI_API_KEY"))
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"cached_input_tokens":0,"output_tokens":1,"reasoning_output_tokens":0}}`)
}

// TestRun_CodexForwardsOpenAIKeyEndToEnd is the cross-boundary assertion
// (cf. #618): it drives run() with --agent codex through the REAL codex
// adapter (only the child binary is faked, via the newCodexInvoker seam),
// and asserts OPENAI_API_KEY flows host-env -> apiKeyForAgent ->
// selectInvoker -> codex.New -> child-process env. The per-package codex
// echo_env test covers the adapter->child env composition for all three
// forwarded vars (OPENAI_API_KEY + the FISHHAWK_* MCP vars); this test
// adds the selection-layer half that no per-package unit exercises.
//
// The sentinel key value is deliberately NOT an `sk-…` shape so the
// redacted --bundle-out variant we read back doesn't scrub it.
// envWithout returns a copy of env with every "KEY=value" entry whose key
// is `key` removed. Used by the cross-boundary codex test to strip
// OPENAI_API_KEY from the faked child's seeded env so the only path that
// can deliver it is the adapter's own append (see the call site).
func envWithout(env []string, key string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func TestRun_CodexForwardsOpenAIKeyEndToEnd(t *testing.T) {
	const sentinel = "openai-sentinel-keyvalue"
	t.Setenv("OPENAI_API_KEY", sentinel)

	origCodex := newCodexInvoker
	newCodexInvoker = func(apiKey string) agent.Invoker {
		c := codex.New(apiKey)
		c.Cmd = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			cc := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcessCodex")
			// Seed the child's env from the host env MINUS OPENAI_API_KEY,
			// plus the helper-process gate. Stripping the sentinel here is
			// what makes the env-forwarding assertion load-bearing: the
			// adapter only re-seeds os.Environ() when cmd.Env is nil, so by
			// pre-setting a non-nil env without OPENAI_API_KEY the ONLY path
			// for the sentinel to reach the child is the cross-boundary seam
			// under test — host-env -> apiKeyForAgent -> codex.New ->
			// adapter cmd.Env append. Were OPENAI_API_KEY left in (as
			// os.Environ() carries it via the t.Setenv above), the child
			// would echo it even if a bug made apiKeyForAgent return "" and
			// the adapter skipped its append, silently passing the test.
			cc.Env = append(envWithout(os.Environ(), "OPENAI_API_KEY"), "GO_HELPER_PROCESS_CODEX=1")
			return cc
		}
		return c
	}
	t.Cleanup(func() { newCodexInvoker = origCodex })

	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", promptPath,
		"--bundle-out", bundlePath,
		"--agent", "codex",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want %d:\n%s", got, exitOK, stderr.String())
	}

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	text, err := gunzip(data)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	if !strings.Contains(string(text), sentinel) {
		t.Errorf("bundle missing forwarded OPENAI_API_KEY value %q; the key did not reach the child env:\n%s", sentinel, text)
	}
}

// TestRun_UnknownAgentFailsBeforeInvocation asserts an unrecognized
// --agent value exits category-A (exitFailure) without ever invoking
// the agent.
func TestRun_UnknownAgentFailsBeforeInvocation(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}

	fake := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, fake)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "plan",
		"--prompt-file", promptPath,
		"--agent", "nope",
	}, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want %d:\n%s", got, exitFailure, stderr.String())
	}
	if fake.callIdx != 0 {
		t.Errorf("agent invoked %d times, want 0 (fail-fast before invocation)", fake.callIdx)
	}
	if !strings.Contains(stderr.String(), `"reason":"agent_select"`) {
		t.Errorf("missing agent_select failure line: %s", stderr.String())
	}
}

func TestRun_BundleWriteFailureSurfacesAsExitFailure(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}

	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid",
		"--backend-url", "https://x",
		"--workflow", "w",
		"--stage", "s",
		"--prompt-file", promptPath,
		// Path under a non-existent parent directory.
		"--bundle-out", filepath.Join(dir, "no-such-subdir", "trace.jsonl.gz"),
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(stderr.String(), `"reason":"bundle_write"`) {
		t.Errorf("missing bundle_write reason: %s", stderr.String())
	}
}

// validPlanJSON returns a minimal standard_v1 plan that the validator
// accepts, sourced from planfixture.Valid() so required-field additions
// are caught by TestValid_SchemaCompliant rather than silently breaking
// these tests. MarshalIndent preserves the "key": "value", substring
// format so existing strings.Replace-based tamper tests still work.
func validPlanJSON() string {
	b, err := json.MarshalIndent(planfixture.Valid(), "", "  ")
	if err != nil {
		panic("planfixture.Valid marshal: " + err.Error())
	}
	return string(b)
}

func TestRun_PlanValidationOK(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	planPath := filepath.Join(dir, "plan.json")
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte(validPlanJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{
			OK:     true,
			Events: []agent.Event{{Kind: "system.init"}},
		},
	})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath, "--plan-out", planPath, "--bundle-out", bundlePath,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want %d:\n%s", got, exitOK, stderr.String())
	}
	// Bundle should now contain the system.init plus a policy_event
	// with outcome=valid.
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	_, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatal(err)
	}
	var sawValid bool
	for _, ev := range events {
		if ev.Kind == "policy_event" && strings.Contains(string(ev.Data), `"outcome":"valid"`) {
			sawValid = true
		}
	}
	if !sawValid {
		t.Errorf("missing policy_event outcome=valid in bundle:\n%+v", events)
	}
}

func TestRun_PlanValidationInvalid_DemotesToCategoryB(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Drop required field "summary" — schema rejects.
	bad := strings.Replace(validPlanJSON(), `"summary": "Add a thing.",`, "", 1)
	if err := os.WriteFile(planPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath, "--plan-out", planPath,
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	out := stderr.String()
	if !strings.Contains(out, `"category":"B"`) {
		t.Errorf("missing category B (plan validation should demote): %s", out)
	}
	if !strings.Contains(out, `"outcome":"failed"`) {
		t.Errorf("missing failed outcome: %s", out)
	}
}

func TestRun_PlanFileMissing_DemotesToCategoryB(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--plan-out", filepath.Join(dir, "nonexistent.json"),
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("missing category B: %s", stderr.String())
	}
}

func TestRun_PlanValidationSkippedOnAgentFailure(t *testing.T) {
	// If the agent already failed (category A), don't run plan
	// validation — there's no plan to validate, and the failure
	// classification must remain A.
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{
			OK:              false,
			FailureCategory: "A",
			FailureReason:   "agent crash",
		},
		returnErr: agent.ErrAgentFailed,
	})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		// plan-out points to a missing file — if validation ran, this
		// would override category to B; assert it doesn't.
		"--plan-out", filepath.Join(dir, "nope.json"),
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("expected category A preserved: %s", stderr.String())
	}
}

func TestRun_ConstraintsConfigUnreadable_DemotesToB(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--constraints-file", filepath.Join(dir, "no-such.json"),
		"--check-base-ref", "main",
	}, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("missing category B: %s", stderr.String())
	}
}

func TestRun_ConstraintsConfigInvalidJSON_DemotesToB(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	configPath := filepath.Join(dir, "constraints.json")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--constraints-file", configPath,
		"--check-base-ref", "main",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("missing category B: %s", stderr.String())
	}
}

func TestRun_ConstraintsRequiresBothFlags(t *testing.T) {
	// --constraints-file alone (no --check-base-ref) should NOT
	// trigger constraint evaluation. Use a path that would error
	// if read; if we still exit OK, the wiring is correct.
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--constraints-file", filepath.Join(dir, "no-such.json"),
		// --check-base-ref intentionally absent.
	}, &stderr)
	if got != exitOK {
		t.Errorf("run = %d, want exitOK (constraints should be skipped without --check-base-ref)", got)
	}
}

func TestRun_ConstraintsSkippedOnAgentFailure(t *testing.T) {
	// If the agent failed, constraint evaluation must not run —
	// keep failure category A.
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{
			OK: false, FailureCategory: "A", FailureReason: "agent crash",
		},
		returnErr: agent.ErrAgentFailed,
	})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--constraints-file", filepath.Join(dir, "no-such.json"),
		"--check-base-ref", "main",
	}, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("expected category A preserved: %s", stderr.String())
	}
}

func TestRun_CheckBaseRefAlone_EmitsDiffEventDoesNotDemote(t *testing.T) {
	// With --check-base-ref set but no --constraints-file, the
	// runner should still attempt to emit a git_diff event (so the
	// backend's policy re-evaluation has data) and NOT demote on
	// diff failure — the customer didn't ask for enforcement, just
	// for the diff to be in the bundle (#247).
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u", "--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--check-base-ref", "main",
		// --constraints-file intentionally absent.
	}, &stderr)
	// tempdir isn't a git repo, so the diff fetch will fail. That
	// emits a `policy_event` with outcome=diff_failed but does NOT
	// demote the run.
	if got != exitOK {
		t.Errorf("run = %d, want exitOK (diff failure shouldn't demote when no constraints-file)", got)
	}
	if strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected NO category B; got: %s", stderr.String())
	}
}

func TestRun_UploadTrace_HappyPath(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{
			OK:     true,
			Events: []agent.Event{{Kind: "system.init"}},
		},
	})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	stageID := "22222222-3333-4444-5555-666666666666"
	runID := "11111111-2222-3333-4444-555555555555"
	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID, "--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--prompt-file", promptPath,
		"--upload-trace",
		"--stage-id", stageID,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotIssueRunID != runID {
		t.Errorf("IssueKey runID = %q, want %q", fu.gotIssueRunID, runID)
	}
	if fu.gotShipArgs == nil {
		t.Fatal("ShipTrace not called")
	}
	if fu.gotShipArgs.StageID != stageID {
		t.Errorf("ShipTrace stage_id = %q, want %q", fu.gotShipArgs.StageID, stageID)
	}
	if fu.gotShipArgs.Variant != "raw" {
		t.Errorf("ShipTrace variant = %q", fu.gotShipArgs.Variant)
	}
	if len(fu.gotShipArgs.Bundle) == 0 {
		t.Error("ShipTrace bundle empty")
	}
	if !strings.Contains(stderr.String(), `"event":"trace_uploaded"`) {
		t.Errorf("missing trace_uploaded log: %s", stderr.String())
	}
	// Per E2.4: every stage ships *both* variants. Raw first
	// (compliance-preferred audit ordering), redacted second (the
	// SPA transcript reads this).
	if len(fu.gotShipCalls) != 2 {
		t.Fatalf("ShipTrace calls = %d, want 2 (raw + redacted)", len(fu.gotShipCalls))
	}
	if fu.gotShipCalls[0].Variant != "raw" {
		t.Errorf("first call variant = %q, want raw", fu.gotShipCalls[0].Variant)
	}
	if fu.gotShipCalls[1].Variant != "redacted" {
		t.Errorf("second call variant = %q, want redacted", fu.gotShipCalls[1].Variant)
	}
	if len(fu.gotShipCalls[1].Bundle) == 0 {
		t.Error("redacted bundle empty")
	}
}

func TestMakeGitDiffEvent_CarriesPatch(t *testing.T) {
	// The git_diff payload must carry the patch + patch_truncated
	// fields so the backend bundle reader (lockstep json tags) can
	// surface them to the implement-review prompt (#585).
	d := constraint.Diff{ChangedFiles: []constraint.ChangedFile{
		{Path: "a.go", Status: constraint.StatusModified},
	}}
	ev := makeGitDiffEvent("main", d, "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-x\n+y\n", true)

	var payload gitDiffPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if !strings.Contains(payload.Patch, "@@ -1 +1 @@") {
		t.Errorf("payload.Patch missing hunk; got %q", payload.Patch)
	}
	if !payload.PatchTruncated {
		t.Error("payload.PatchTruncated = false, want true")
	}
	// Absent patch round-trips to the empty/false zero values.
	ev2 := makeGitDiffEvent("main", d, "", false)
	var payload2 gitDiffPayload
	if err := json.Unmarshal(ev2.Payload, &payload2); err != nil {
		t.Fatalf("unmarshal payload2: %v", err)
	}
	if payload2.Patch != "" || payload2.PatchTruncated {
		t.Errorf("absent patch should be empty/false; got %+v", payload2)
	}
}

// TestRun_GitDiffPatch_RedactedInBundle drives the full runner with a
// real git repo whose staged change adds a line containing a secret.
// The patch lands inside the git_diff event payload, so it must be
// redacted in the redacted bundle variant while staying verbatim in the
// raw variant (#585 binding condition 2).
func TestRun_GitDiffPatch_RedactedInBundle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "--initial-branch=main")
	runGit("config", "user.name", "init")
	runGit("config", "user.email", "init@example.com")
	runGit("config", "commit.gpgsign", "false")
	runGit("config", "tag.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "config.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "initial")
	runGit("branch", "base")

	// Agent-style edit adding a credential line. The runner stages it
	// via `git add -A` inside computeAndEmitDiff before computing the
	// patch, so the secret appears as an added (`+`) hunk line.
	secret := "ghp_" + strings.Repeat("a", 36)
	if err := os.WriteFile(filepath.Join(repo, "config.go"),
		[]byte("package x\nconst token = \""+secret+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Prompt file lives outside the repo so it doesn't pollute the diff.
	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	stageID := "22222222-3333-4444-5555-666666666666"
	runID := "11111111-2222-3333-4444-555555555555"
	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID, "--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--prompt-file", promptPath,
		"--working-dir", repo,
		"--check-base-ref", "base",
		"--upload-trace",
		"--stage-id", stageID,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if len(fu.gotShipCalls) != 2 {
		t.Fatalf("ShipTrace calls = %d, want 2 (raw, redacted)", len(fu.gotShipCalls))
	}

	raw, err := gunzip(fu.gotShipCalls[0].Bundle)
	if err != nil {
		t.Fatalf("gunzip raw: %v", err)
	}
	redacted, err := gunzip(fu.gotShipCalls[1].Bundle)
	if err != nil {
		t.Fatalf("gunzip redacted: %v", err)
	}
	// Raw bundle must carry the patch verbatim — proving the patch was
	// captured into the git_diff event and the secret reached the wire.
	if !bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("secret not in raw bundle; patch not captured? raw:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte(`"patch"`)) {
		t.Errorf("raw bundle git_diff event missing patch field")
	}
	// Redacted bundle must NOT carry the secret.
	if bytes.Contains(redacted, []byte(secret)) {
		t.Errorf("secret survived redaction in redacted bundle")
	}
	if !bytes.Contains(redacted, []byte("[REDACTED:github-pat-classic]")) {
		t.Errorf("redaction marker missing from redacted bundle")
	}
}

func TestRun_UploadTrace_RedactsSecrets(t *testing.T) {
	// Plant a known credential in an event payload; the redacted
	// variant must not contain it after re-pack. This is the
	// regression guard for E2.4 — the redacted bundle is the SPA
	// transcript's only input, so leakage here ends up readable
	// behind cookie auth.
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}

	// ghp_… is in DefaultPatterns; using a 36-char alphanumeric
	// suffix matches the regex exactly. The token bytes are
	// deliberate — anything that wouldn't match the pattern would
	// pass through and the test would assert against itself.
	secret := "ghp_" + strings.Repeat("a", 36)
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{
			OK: true,
			Events: []agent.Event{
				{Kind: "raw", Payload: json.RawMessage(`{"text":"saw ` + secret + ` in stderr"}`)},
			},
		},
	})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	stageID := "22222222-3333-4444-5555-666666666666"
	runID := "11111111-2222-3333-4444-555555555555"
	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID, "--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--prompt-file", promptPath,
		"--upload-trace",
		"--stage-id", stageID,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if len(fu.gotShipCalls) != 2 {
		t.Fatalf("ShipTrace calls = %d, want 2", len(fu.gotShipCalls))
	}

	rawHasSecret := bytes.Contains(fu.gotShipCalls[0].Bundle, []byte(secret))
	redactedHasSecret := bytes.Contains(fu.gotShipCalls[1].Bundle, []byte(secret))
	if !rawHasSecret {
		// If the secret didn't make it into raw either, the test
		// is broken — likely the bundle's gzipped and we'd need to
		// decompress to find it. Decompress to be robust.
		raw, err := gunzip(fu.gotShipCalls[0].Bundle)
		if err != nil {
			t.Fatalf("raw bundle gunzip: %v", err)
		}
		if !bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("secret not in raw bundle either; test broken")
		}
		// Compare against decompressed bytes for redacted too.
		redacted, err := gunzip(fu.gotShipCalls[1].Bundle)
		if err != nil {
			t.Fatalf("redacted bundle gunzip: %v", err)
		}
		if bytes.Contains(redacted, []byte(secret)) {
			t.Errorf("secret survived redaction in decompressed redacted bundle")
		}
		if !bytes.Contains(redacted, []byte("[REDACTED:github-pat-classic]")) {
			t.Errorf("redaction marker missing from redacted bundle")
		}
	} else if redactedHasSecret {
		t.Errorf("secret survived redaction in redacted bundle")
	}

	// Telemetry: a trace_redacted log line with a non-empty hits
	// list, so operators can see "the runner redacted N tokens this
	// run" without leaking the tokens themselves.
	if !strings.Contains(stderr.String(), `"event":"trace_redacted"`) {
		t.Errorf("missing trace_redacted telemetry log:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"github-pat-classic"`) {
		t.Errorf("trace_redacted log should name the matched pattern:\n%s", stderr.String())
	}
}

func TestRun_UploadTrace_RequiresStageID(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	withFakeUploader(t, newFakeUploader(t))

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--upload-trace",
		// no --stage-id
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "stage-id required") {
		t.Errorf("missing stage-id required: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("missing category C: %s", stderr.String())
	}
}

func TestRun_UploadTrace_IssueKeyFailure_CategoryC(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.issueErr = errors.New("backend unreachable")
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--upload-trace",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("missing category C: %s", stderr.String())
	}
}

func TestRun_UploadTrace_ShipFailure_CategoryC(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.shipErr = errors.New("disk full on backend")
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--upload-trace",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("missing category C: %s", stderr.String())
	}
}

func TestRun_UploadTrace_DoesntOverwriteCategoryB(t *testing.T) {
	// If plan validation already demoted to B, an upload failure
	// must not overwrite the category.
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	planPath := filepath.Join(dir, "plan.json")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)
	_ = os.WriteFile(planPath, []byte("{not a plan}"), 0o600)

	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--plan-out", planPath,
		"--upload-trace",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	// Category should still be B (plan validation failure), not C.
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category B preserved: %s", stderr.String())
	}
}

func TestEmitEvents_OneJSONPerLine(t *testing.T) {
	var w bytes.Buffer
	emitEvents(&w, []agent.Event{
		{Kind: "a"},
		{Kind: "b", Payload: agent.MakePayload(map[string]int{"n": 1})},
	})
	lines := bytes.Split(bytes.TrimRight(w.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), w.String())
	}
	for _, line := range lines {
		var ev agent.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Errorf("line not JSON: %v: %s", err, line)
		}
	}
}

func TestNewInvoker_DefaultIsClaudeCode(t *testing.T) {
	// Sanity check: production wiring constructs a non-nil invoker.
	// Regression guard for someone removing the default assignment.
	inv := newInvoker("k")
	if inv == nil {
		t.Fatal("newInvoker returned nil")
	}
}

func TestLogCompletion_FailureFallsBackToErrText(t *testing.T) {
	// FailureReason empty → reason should fall back to err.Error().
	var w strings.Builder
	logCompletion(&w, agent.Result{OK: false}, errors.New("boom"))
	out := w.String()
	if !strings.Contains(out, `"reason":"boom"`) {
		t.Errorf("missing reason fallback: %s", out)
	}
	// FailureCategory empty → should default to "A".
	if !strings.Contains(out, `"category":"A"`) {
		t.Errorf("missing default category A: %s", out)
	}
}

func TestRun_FetchPrompt_HappyPath(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
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
	if fu.gotPromptArgs == nil {
		t.Fatal("FetchPrompt not called")
	}
	if fu.gotPromptArgs.StageID != "22222222-3333-4444-5555-666666666666" {
		t.Errorf("FetchPrompt stage_id = %q", fu.gotPromptArgs.StageID)
	}
	if !strings.Contains(stderr.String(), `"event":"prompt_fetched"`) {
		t.Errorf("missing prompt_fetched log: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"signing_key_issued"`) {
		t.Errorf("missing signing_key_issued log: %s", stderr.String())
	}
}

// TestRun_FetchPrompt_FetchesMCPToken_AndStampsAgentEnv covers the
// E19.8 / #348 wiring: after the per-run signing key is issued,
// the runner fetches an MCP token via FetchMCPToken and layers
// FISHHAWK_API_TOKEN + FISHHAWK_BACKEND_URL onto the agent
// Invocation.Env. The fake uploader returns "fhm_stubmcptoken…"
// by default; assertions check both the call site and the
// downstream env wiring.
func TestRun_FetchPrompt_FetchesMCPToken_AndStampsAgentEnv(t *testing.T) {
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "Hello agent.",
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
	if fu.gotMCPTokenArgs == nil {
		t.Fatal("FetchMCPToken not called")
	}
	if fu.gotMCPTokenArgs.RunID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("FetchMCPToken RunID = %q", fu.gotMCPTokenArgs.RunID)
	}
	if invoker.gotInv == nil {
		t.Fatal("invoker.gotInv nil — invocation not captured")
	}
	if got := invoker.gotInv.Env["FISHHAWK_API_TOKEN"]; got != "fhm_stubmcptokenforuse" {
		t.Errorf("FISHHAWK_API_TOKEN = %q, want stub", got)
	}
	if got := invoker.gotInv.Env["FISHHAWK_BACKEND_URL"]; got != "https://api.fishhawk.test" {
		t.Errorf("FISHHAWK_BACKEND_URL = %q", got)
	}
	if !strings.Contains(stderr.String(), `"event":"mcp_token_issued"`) {
		t.Errorf("missing mcp_token_issued log line: %s", stderr.String())
	}
}

// TestRun_FetchPrompt_MCPTokenFetchFailure_StillProceeds locks
// in the best-effort posture: if FetchMCPToken errors, the runner
// logs the failure but continues invoking the agent (without the
// FISHHAWK_API_TOKEN env). Aligns with ADR-021's "MCP awareness
// is best-effort" framing.
func TestRun_FetchPrompt_MCPTokenFetchFailure_StillProceeds(t *testing.T) {
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "Hello agent.",
		PromptHash: "deadbeef",
	}
	fu.mcpTokenErr = errors.New("backend offline")
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
		t.Fatalf("run = %d, want exitOK (failure is best-effort):\n%s", got, stderr.String())
	}
	if invoker.gotInv == nil {
		t.Fatal("invoker.gotInv nil")
	}
	if _, ok := invoker.gotInv.Env["FISHHAWK_API_TOKEN"]; ok {
		t.Errorf("FISHHAWK_API_TOKEN should be absent when fetch failed; got %q",
			invoker.gotInv.Env["FISHHAWK_API_TOKEN"])
	}
	if !strings.Contains(stderr.String(), `"event":"mcp_token_fetch_failed"`) {
		t.Errorf("missing mcp_token_fetch_failed log line: %s", stderr.String())
	}
}

// TestRun_WiresProgressSinkToLogSink confirms run() sets the
// Invocation's ProgressSink to the same logSink it writes lifecycle
// lines to, so the agent adapter's stage_progress heartbeats land on
// the stream the fishhawk-mcp relay forwards (#580).
func TestRun_WiresProgressSinkToLogSink(t *testing.T) {
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "Hello agent.",
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
		t.Fatal("invoker.gotInv nil — invocation not captured")
	}
	if invoker.gotInv.ProgressSink != io.Writer(&stderr) {
		t.Errorf("ProgressSink = %v, want the logSink passed to run()", invoker.gotInv.ProgressSink)
	}
}

func TestRun_FetchPrompt_PromptFileWins(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("local override"), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--stage-id", "stage-1",
		"--fetch-prompt",
		"--prompt-file", promptPath,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotPromptArgs != nil {
		t.Errorf("FetchPrompt called even though --prompt-file was set")
	}
}

func TestRun_FetchPrompt_RequiresStageID(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	withFakeUploader(t, newFakeUploader(t))

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--fetch-prompt",
		// no --stage-id
	}, &stderr)
	if got != exitUsage {
		t.Errorf("run = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "stage-id") {
		t.Errorf("missing stage-id error: %s", stderr.String())
	}
}

func TestRun_FetchPrompt_FetchFailure(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptErr = errors.New("backend down")
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--stage-id", "stage-1",
		"--fetch-prompt",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "fetch_prompt") {
		t.Errorf("missing fetch_prompt error: %s", stderr.String())
	}
}

func TestRun_FetchPrompt_IssueKeyFailure(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.issueErr = errors.New("issue failed")
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--stage-id", "stage-1",
		"--fetch-prompt",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "issue_key") {
		t.Errorf("missing issue_key error: %s", stderr.String())
	}
}

func TestRun_FetchPrompt_PlusUploadTrace_OnlyOneIssueKeyCall(t *testing.T) {
	// Critical invariant: signing-key endpoint is one-shot per run.
	// Combining --fetch-prompt with --upload-trace must reuse the
	// same key issued at fetch-prompt time, not call IssueKey twice.
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true,
		Events: []agent.Event{{Kind: "system.init"}}}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--stage-id", "stage-1",
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotIssueCount != 1 {
		t.Errorf("IssueKey calls = %d, want 1", fu.gotIssueCount)
	}
	if fu.gotPromptArgs == nil {
		t.Error("FetchPrompt not called")
	}
	if fu.gotShipArgs == nil {
		t.Error("ShipTrace not called")
	}
}

func TestRun_UploadPlan_HappyPath(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, []byte(validPlanJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	stageID := "22222222-3333-4444-5555-666666666666"
	runID := "11111111-2222-3333-4444-555555555555"
	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID, "--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--prompt-file", promptPath,
		"--plan-out", planPath,
		"--upload-trace",
		"--stage-id", stageID,
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	// Both trace and plan should have been shipped, in that order.
	if fu.gotShipArgs == nil {
		t.Fatal("ShipTrace not called")
	}
	if fu.gotPlanArgs == nil {
		t.Fatal("ShipPlan not called")
	}
	if fu.gotPlanArgs.StageID != stageID {
		t.Errorf("ShipPlan stage_id = %q, want %q", fu.gotPlanArgs.StageID, stageID)
	}
	if fu.gotPlanArgs.RunID != runID {
		t.Errorf("ShipPlan run_id = %q, want %q", fu.gotPlanArgs.RunID, runID)
	}
	if !strings.HasPrefix(string(fu.gotPlanArgs.Plan), `{`) {
		t.Errorf("ShipPlan plan body should be JSON, got prefix %q", string(fu.gotPlanArgs.Plan)[:1])
	}

	// Single-issue: signing-key issued exactly once across the whole
	// run (prompt fetch / trace upload / plan upload share it).
	if fu.gotIssueCount != 1 {
		t.Errorf("IssueKey called %d times, want 1", fu.gotIssueCount)
	}

	if !strings.Contains(stderr.String(), `"event":"plan_uploaded"`) {
		t.Errorf("missing plan_uploaded log:\n%s", stderr.String())
	}
}

func TestRun_UploadPlan_NetworkError_CategoryC(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	planPath := filepath.Join(dir, "plan.json")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)
	_ = os.WriteFile(planPath, []byte(validPlanJSON()), 0o600)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.planErr = errors.New("ship plan: connection refused")
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--plan-out", planPath,
		"--upload-trace",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"reason":"plan_upload"`) {
		t.Errorf("missing plan_upload reason:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("expected category-C on network error, got:\n%s", stderr.String())
	}
}

func TestRun_UploadPlan_PlanInvalid_CategoryB(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	planPath := filepath.Join(dir, "plan.json")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)
	_ = os.WriteFile(planPath, []byte(validPlanJSON()), 0o600)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.planErr = upload.ErrPlanInvalid
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--plan-out", planPath,
		"--upload-trace",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category-B on schema reject, got:\n%s", stderr.String())
	}
}

func TestRun_PlanValidationInvalid_ShipsToBackend(t *testing.T) {
	// #613: when local plan-validation fails, the runner must STILL POST
	// the invalid plan (with --upload-trace) so the backend's
	// handleShipPlan accept-and-reject path owns the running->failed(B)
	// transition rather than leaving the stage in `running` until the SLA
	// watchdog reaps it.
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(promptPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Drop required field "summary" — schema rejects (a violation class
	// coercion cannot synthesize), mirroring
	// TestRun_PlanValidationInvalid_DemotesToCategoryB.
	bad := strings.Replace(validPlanJSON(), `"summary": "Add a thing.",`, "", 1)
	if err := os.WriteFile(planPath, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	// Mirror the backend's 400 plan_invalid response on the shipped body.
	fu.planErr = upload.ErrPlanInvalid
	withFakeUploader(t, fu)

	runID := "11111111-2222-3333-4444-555555555555"
	stageID := "22222222-3333-4444-5555-666666666666"
	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID, "--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--prompt-file", promptPath,
		"--plan-out", planPath,
		"--upload-trace",
		"--stage-id", stageID,
	}, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	out := stderr.String()
	// Regression guard (#613): ShipPlan WAS called with the invalid plan.
	if fu.gotPlanArgs == nil {
		t.Fatal("ShipPlan not called — locally-invalid plan must still be shipped")
	}
	if !strings.Contains(out, `"event":"plan_invalid_shipped"`) {
		t.Errorf("missing plan_invalid_shipped log:\n%s", out)
	}
	if !strings.Contains(out, `"category":"B"`) {
		t.Errorf("expected category-B propagated from backend reject:\n%s", out)
	}
}

func TestRun_UploadPlan_NotShippedWithoutPlanOut(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--upload-trace",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK", got)
	}
	if fu.gotPlanArgs != nil {
		t.Error("ShipPlan should not be called without --plan-out")
	}
}

// fakePusher captures CommitAndPush args and returns canned results.
type fakePusher struct {
	gotArgs *gitops.CommitAndPushArgs
	result  *gitops.CommitAndPushResult
	err     error

	// errSeq, when non-empty, takes precedence over err: call i returns
	// errSeq[i] (the last entry repeats after the slice is exhausted); a nil
	// entry falls through to result. The base-rebase-conflict re-invoke
	// tests (#989) use it to fail the first CommitAndPush and succeed on the
	// retry.
	errSeq []error

	// calls counts CommitAndPush invocations so tests can assert the retry
	// is bounded.
	calls int
}

func (f *fakePusher) CommitAndPush(_ context.Context, args gitops.CommitAndPushArgs) (*gitops.CommitAndPushResult, error) {
	a := args
	f.gotArgs = &a
	idx := f.calls
	f.calls++
	if len(f.errSeq) > 0 {
		if idx >= len(f.errSeq) {
			idx = len(f.errSeq) - 1
		}
		if err := f.errSeq[idx]; err != nil {
			return nil, err
		}
	} else if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &gitops.CommitAndPushResult{
		HeadSHA: "head-sha-abc",
		BaseSHA: "base-sha-def",
	}, nil
}

// fakePROpener captures OpenPR args and returns canned results.
type fakePROpener struct {
	gotArgs  *gitops.OpenPRArgs
	gotToken string
	result   *gitops.OpenPRResult
	err      error
}

func (f *fakePROpener) OpenPR(_ context.Context, args gitops.OpenPRArgs) (*gitops.OpenPRResult, error) {
	a := args
	f.gotArgs = &a
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &gitops.OpenPRResult{
		PRNumber: 42,
		PRURL:    "https://github.com/x/y/pull/42",
	}, nil
}

func withFakeGitOps(t *testing.T, fp *fakePusher, fpr *fakePROpener) {
	t.Helper()
	origP, origO := newPusher, newPROpener
	origCap, origRes := captureHead, restoreHead
	origCheckout := checkoutFixupBase
	origRunBranch := checkoutRunBranch
	origDirty, origClean, origResPres := dirtyPaths, cleanDriftPaths, restoreHeadPreserving
	newPusher = func() pusher { return fp }
	newPROpener = func(token string) prOpener {
		fpr.gotToken = token
		return fpr
	}
	// Stub the working-tree restoration seam (#911) to safe no-ops so a
	// fake-pusher run() test — repoDir defaults to "." (the runner's own
	// source repo) — never runs `git checkout --force` against the real
	// working tree. captureHead returns a sentinel so the defer still
	// installs (exercising the emit path); restoreHead does nothing. Tests
	// that need to assert the wiring swap in recording spies AFTER this.
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		return "operator-branch", false, nil
	}
	restoreHead = func(_ context.Context, _, _ string) error { return nil }
	// Stub the fix-up base establishment (#967) the same way: a fixup run()
	// test must never fetch + force-checkout the runner's own source repo.
	// The canned tip flows into the lineage comparison; tests that assert
	// the wiring swap in recording spies AFTER this.
	checkoutFixupBase = func(_ context.Context, _, _, _ string) (string, error) {
		return "fixup-branch-tip-sha", nil
	}
	// Stub the base-rebase-conflict re-checkout (#989) the same way: the
	// re-invoke handler must never force-checkout the runner's own source
	// repo in a fake-pusher run() test.
	checkoutRunBranch = func(_ context.Context, _, _ string) error { return nil }
	// Stub the #943 drift-cleanup seam: dirtyPaths reports a clean pre-agent
	// tree, cleanDriftPaths and restoreHeadPreserving do nothing — a
	// fake-pusher run() test must never stash/checkout the runner's own
	// source repo. Tests that assert the partition wiring swap in recording
	// spies AFTER this.
	dirtyPaths = func(_ context.Context, _ string) ([]string, error) { return nil, nil }
	cleanDriftPaths = func(_ context.Context, _ string, _ []string) error { return nil }
	restoreHeadPreserving = func(_ context.Context, _, _ string, _ []string) error { return nil }
	t.Cleanup(func() {
		newPusher = origP
		newPROpener = origO
		captureHead = origCap
		restoreHead = origRes
		checkoutFixupBase = origCheckout
		checkoutRunBranch = origRunBranch
		dirtyPaths = origDirty
		cleanDriftPaths = origClean
		restoreHeadPreserving = origResPres
	})
}

// implementEnv sets the env vars the implement-stage flow reads
// from the Actions environment. The App-token migration (#197) made
// GITHUB_TOKEN unused; we keep setting it for parity with the live
// Actions environment (the runner doesn't read it any more).
// Restored via t.Cleanup.
func implementEnv(t *testing.T, repo, ref string) {
	t.Helper()
	t.Setenv("GITHUB_REPOSITORY", repo)
	t.Setenv("GITHUB_REF_NAME", ref)
}

// TestRun_ImplementStage_ScopeFilesThreadedAndHandoffWritten verifies
// the #581 plumbing: the prompt response's scope_files are threaded
// into CommitAndPushArgs.ScopeFiles (as repo-relative paths) and
// written to the /tmp/fishhawk-scope.json handoff for the CLI auto-PR
// path.
func TestRun_ImplementStage_ScopeFilesThreadedAndHandoffWritten(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	stageID := "22222222-3333-4444-5555-666666666666"
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    stageID,
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
		ScopeFiles: []upload.ScopeFile{
			{Path: "a/b.go", Operation: "modify"},
			{Path: "c/d.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	// Redirect the scope handoff to a temp path so the CLI auto-PR
	// contract is exercised without /tmp pollution.
	scopePath := filepath.Join(t.TempDir(), "scope.json")
	origScope := scopeHandoffPath
	scopeHandoffPath = scopePath
	t.Cleanup(func() { scopeHandoffPath = origScope })

	runID := "11111111-2222-3333-4444-555555555555"
	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	wantPaths := []string{"a/b.go", "c/d.go"}
	if len(fp.gotArgs.ScopeFiles) != len(wantPaths) {
		t.Fatalf("CommitAndPush ScopeFiles = %v, want %v", fp.gotArgs.ScopeFiles, wantPaths)
	}
	for i, want := range wantPaths {
		if fp.gotArgs.ScopeFiles[i] != want {
			t.Errorf("ScopeFiles[%d] = %q, want %q", i, fp.gotArgs.ScopeFiles[i], want)
		}
	}

	// The handoff file was written for the out-of-process CLI path.
	data, err := os.ReadFile(scopePath)
	if err != nil {
		t.Fatalf("read scope handoff: %v", err)
	}
	var sh scopeHandoff
	if err := json.Unmarshal(data, &sh); err != nil {
		t.Fatalf("decode scope handoff: %v", err)
	}
	if len(sh.Files) != 2 || sh.Files[0].Path != "a/b.go" || sh.Files[1].Operation != "create" {
		t.Errorf("scope handoff files = %+v", sh.Files)
	}
}

// TestRun_ImplementStage_NoScopeFiles_FallsBack confirms that when the
// prompt response carries no scope_files, CommitAndPush gets an empty
// ScopeFiles (the `git add -A` fallback) and no handoff file is written.
func TestRun_ImplementStage_NoScopeFiles_FallsBack(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	stageID := "22222222-3333-4444-5555-666666666666"
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    stageID,
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	scopePath := filepath.Join(t.TempDir(), "scope.json")
	origScope := scopeHandoffPath
	scopeHandoffPath = scopePath
	t.Cleanup(func() { scopeHandoffPath = origScope })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if len(fp.gotArgs.ScopeFiles) != 0 {
		t.Errorf("ScopeFiles = %v, want empty (fallback to git add -A)", fp.gotArgs.ScopeFiles)
	}
	if _, err := os.Stat(scopePath); !os.IsNotExist(err) {
		t.Errorf("scope handoff should not be written when scope is empty; stat err = %v", err)
	}
}

// TestRun_ImplementStage_CommitAuthorThreaded verifies the #722 plumbing:
// the prompt response's commit_author_name/commit_author_email are threaded
// into CommitAndPushArgs.AuthorName/AuthorEmail so App-backed commits
// attribute to the App's bot account.
func TestRun_ImplementStage_CommitAuthorThreaded(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	stageID := "22222222-3333-4444-5555-666666666666"
	fu.promptResp = &upload.FetchedPrompt{
		StageID:           stageID,
		StageType:         "implement",
		Prompt:            "implement",
		PromptHash:        "h",
		CommitAuthorName:  "fishhawk[bot]",
		CommitAuthorEmail: "41898282+fishhawk[bot]@users.noreply.github.com",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if fp.gotArgs.AuthorName != "fishhawk[bot]" {
		t.Errorf("AuthorName = %q, want fishhawk[bot]", fp.gotArgs.AuthorName)
	}
	if fp.gotArgs.AuthorEmail != "41898282+fishhawk[bot]@users.noreply.github.com" {
		t.Errorf("AuthorEmail = %q", fp.gotArgs.AuthorEmail)
	}
}

// TestRun_ImplementStage_NoCommitAuthor_FallsBack confirms that when the
// prompt response omits the commit author identity, CommitAndPush receives
// empty AuthorName/AuthorEmail so gitops applies its default bot identity.
func TestRun_ImplementStage_NoCommitAuthor_FallsBack(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	stageID := "22222222-3333-4444-5555-666666666666"
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    stageID,
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if fp.gotArgs.AuthorName != "" || fp.gotArgs.AuthorEmail != "" {
		t.Errorf("Author identity = (%q,%q), want empty (gitops default fallback)",
			fp.gotArgs.AuthorName, fp.gotArgs.AuthorEmail)
	}
}

func TestRun_ImplementStage_HappyPath(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	runID := "11111111-2222-3333-4444-555555555555"
	stageID := "22222222-3333-4444-5555-666666666666"
	var stderr strings.Builder
	got := run([]string{
		"--run-id", runID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", stageID,
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if !strings.HasPrefix(fp.gotArgs.Branch, "fishhawk/run-11111111/stage-22222222") {
		t.Errorf("branch = %q, want fishhawk/run-<short>/stage-<short>", fp.gotArgs.Branch)
	}
	if fp.gotArgs.RemoteURL != "https://github.com/kuhlman-labs/fishhawk" {
		t.Errorf("RemoteURL = %q", fp.gotArgs.RemoteURL)
	}
	// Push auth flows through actions/checkout's extraheader (set
	// by the auth pre-step in the workflow per #201), not through
	// gitops args.

	if fpr.gotArgs == nil {
		t.Fatal("OpenPR not called")
	}
	if fpr.gotArgs.Owner != "kuhlman-labs" || fpr.gotArgs.Repo != "fishhawk" {
		t.Errorf("owner/repo = %q/%q", fpr.gotArgs.Owner, fpr.gotArgs.Repo)
	}
	if fpr.gotArgs.Base != "main" {
		t.Errorf("base = %q", fpr.gotArgs.Base)
	}
	if fpr.gotToken != "ghs_app_token" {
		t.Errorf("PROpener token = %q, want ghs_app_token (the App installation token)", fpr.gotToken)
	}

	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest not called")
	}
	var sent map[string]any
	if err := json.Unmarshal(fu.gotPRArgs.Body, &sent); err != nil {
		t.Fatalf("decode shipped body: %v", err)
	}
	for _, want := range []string{"pr_number", "pr_url", "branch", "head_sha", "base_sha", "title"} {
		if _, ok := sent[want]; !ok {
			t.Errorf("shipped body missing %q: %+v", want, sent)
		}
	}

	if !strings.Contains(stderr.String(), `"event":"pull_request_opened"`) ||
		!strings.Contains(stderr.String(), `"event":"pull_request_uploaded"`) {
		t.Errorf("missing pr lifecycle log lines:\n%s", stderr.String())
	}
}

func TestRun_ImplementStage_NoChanges_SkipsPRAndShip(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{result: &gitops.CommitAndPushResult{NoChanges: true, BaseSHA: "base"}}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR should not be called when there are no changes")
	}
	if fu.gotPRArgs != nil {
		t.Error("ShipPullRequest should not be called when there are no changes")
	}
	if !strings.Contains(stderr.String(), `"event":"implement_no_changes"`) {
		t.Errorf("missing implement_no_changes log:\n%s", stderr.String())
	}
}

func TestRun_ImplementStage_PushError_CategoryC(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: errors.New("push: bad credentials")}
	withFakeGitOps(t, fp, &fakePROpener{})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("expected category-C on push error, got:\n%s", stderr.String())
	}
}

func TestRun_ImplementStage_ShipPRInvalid_CategoryB(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	fu.prErr = upload.ErrPullRequestInvalid
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category-B on schema reject, got:\n%s", stderr.String())
	}
}

func TestRun_ImplementStage_PlanOutWithImplementStage_DoesNotValidatePlan(t *testing.T) {
	// Workflow file passes plan-out unconditionally; for an
	// implement stage there's no plan file at that path. The
	// runner must not fail-by-default — it should skip plan
	// validation entirely.
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	dir := t.TempDir()
	missingPlan := filepath.Join(dir, "no-such-plan.json")
	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
		"--plan-out", missingPlan, // file doesn't exist; would fail validation pre-fix
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (plan validation should be skipped for implement stage):\n%s", got, stderr.String())
	}
	if fu.gotPlanArgs != nil {
		t.Error("ShipPlan should not be called for implement stage")
	}
}

func TestRun_ImplementStage_InstallationTokenFetchFails_CategoryC(t *testing.T) {
	// Backend returns 502 (or any error) when fetching the App
	// token. The implement stage should abort with category-C
	// before any git push or PR is attempted.
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	fu.instTokenErr = errors.New("backend: installation_token_issuance_failed")
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("expected category-C on token-fetch error, got:\n%s", stderr.String())
	}
	if fp.gotArgs != nil {
		t.Error("CommitAndPush should not be called when token fetch fails")
	}
}

// withFakeGHAuthToken swaps the ghAuthToken seam for the duration of a
// test, restoring it on cleanup.
func withFakeGHAuthToken(t *testing.T, fn func(context.Context) (string, error)) {
	t.Helper()
	orig := ghAuthToken
	ghAuthToken = fn
	t.Cleanup(func() { ghAuthToken = orig })
}

func TestRun_ImplementStage_NoInstallation_FallsBackToGHToken(t *testing.T) {
	// A local / MCP run on a repo with no App installation: the backend
	// returns ErrNoInstallation, and the runner sources a token from the
	// operator's `gh` CLI and threads it through push + PR (#713).
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	fu.instTokenErr = fmt.Errorf("%w: no_installation_for_run", upload.ErrNoInstallation)
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)
	withFakeGHAuthToken(t, func(context.Context) (string, error) { return "gho_local_token", nil })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if fp.gotArgs.PushToken != "gho_local_token" {
		t.Errorf("PushToken = %q, want gho_local_token (the gh CLI fallback token)", fp.gotArgs.PushToken)
	}
	if fpr.gotToken != "gho_local_token" {
		t.Errorf("OpenPR token = %q, want gho_local_token", fpr.gotToken)
	}
	if !strings.Contains(stderr.String(), `"source":"gh_cli"`) {
		t.Errorf("expected installation_token_received with source gh_cli, got:\n%s", stderr.String())
	}
}

func TestRun_ImplementStage_NoInstallation_NoGHToken_Actionable(t *testing.T) {
	// No App installation AND no usable `gh` CLI token: the runner must
	// fail with a clear, actionable error naming the two fixes — never
	// the opaque "fetch installation token" wrap — and must not open a PR.
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	fu.instTokenErr = fmt.Errorf("%w: no_installation_for_run", upload.ErrNoInstallation)
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)
	withFakeGHAuthToken(t, func(context.Context) (string, error) {
		return "", errors.New("gh: not logged in")
	})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	out := stderr.String()
	if !strings.Contains(out, "gh auth login") || !strings.Contains(out, "GitHub App") {
		t.Errorf("expected actionable error naming the App install + `gh auth login`, got:\n%s", out)
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR should not be called when no fallback token is available")
	}
}

func TestRun_ImplementStage_AlwaysFetchesFreshTokenBeforePush(t *testing.T) {
	// Even with FISHHAWK_GITHUB_TOKEN set in env (the auth pre-
	// step's pass-through), the runner always mints a fresh token
	// before push so a long agent run can outlive the original
	// token's 1-hour TTL. The audit chain gets two
	// installation_token_issued events per implement stage: the
	// OIDC one at workflow start, the Ed25519 one here.
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	t.Setenv("FISHHAWK_GITHUB_TOKEN", "stale-pre-step-token")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	// Always-fetch invariant: the runner called FetchInstallationToken
	// regardless of FISHHAWK_GITHUB_TOKEN being set in env.
	if fu.gotInstTokenArgs == nil {
		t.Error("FetchInstallationToken must be called every implement stage to get a non-expired token")
	}
	// The fresh token from the backend (not the env one) reaches
	// gitops as PushToken and the PR opener.
	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if fp.gotArgs.PushToken != "ghs_app_token" {
		t.Errorf("PushToken = %q, want ghs_app_token (the fresh backend-minted token, NOT the stale env token)", fp.gotArgs.PushToken)
	}
	if fpr.gotToken != "ghs_app_token" {
		t.Errorf("PROpener token = %q, want ghs_app_token", fpr.gotToken)
	}
}

// withPRDescriptionPath redirects pullRequestDescriptionPath to a
// temp dir for the duration of the test, restoring the production
// value via t.Cleanup. Tests that write to the file should call
// this before writing.
func withPRDescriptionPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fishhawk-pr.md")
	orig := pullRequestDescriptionPath
	pullRequestDescriptionPath = path
	t.Cleanup(func() { pullRequestDescriptionPath = orig })
	return path
}

func TestPRTitleAndBody_AgentAuthored_HappyPath(t *testing.T) {
	path := withPRDescriptionPath(t)
	if err := os.WriteFile(path, []byte("Add make minio-init target\n\n## Why\n\nLocal stack needed a bucket-init step.\n\nCloses #184\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config{
		runID:      "11111111-2222-3333-4444-555555555555",
		stageID:    "22222222-3333-4444-5555-666666666666",
		backendURL: "https://api.fishhawk.test",
	}
	var stderr strings.Builder
	title, body := prTitleAndBody(cfg, "fishhawk/run-x/stage-y", &stderr)
	if title != "Add make minio-init target" {
		t.Errorf("title = %q, want agent's first line", title)
	}
	if !strings.Contains(body, "## Why") {
		t.Errorf("body should preserve agent markdown, got:\n%s", body)
	}
	if !strings.Contains(body, "Closes #184") {
		t.Errorf("body should include the Closes line, got:\n%s", body)
	}
	// Footer must be appended for audit-trail provenance.
	if !strings.Contains(body, "Audit log:") || !strings.Contains(body, cfg.runID) {
		t.Errorf("body missing Fishhawk attribution footer:\n%s", body)
	}
	// No template-warning logs on the happy path.
	if strings.Contains(stderr.String(), "pr_template_invalid") || strings.Contains(stderr.String(), "pr_template_warning") {
		t.Errorf("happy path should produce no template warnings, got:\n%s", stderr.String())
	}
}

func TestPRTitleAndBody_AgentAuthored_TitleOnly(t *testing.T) {
	path := withPRDescriptionPath(t)
	if err := os.WriteFile(path, []byte("Just the title\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	title, body := prTitleAndBody(config{
		runID: "r", stageID: "s", backendURL: "https://x",
	}, "branch", &stderr)
	if title != "Just the title" {
		t.Errorf("title = %q", title)
	}
	// Body is empty + footer; the title-only case is allowed.
	if !strings.Contains(body, "Audit log:") {
		t.Errorf("footer should still be appended, got:\n%s", body)
	}
	// Should warn (not error) because title-only is non-canonical.
	if !strings.Contains(stderr.String(), "pr_template_warning") {
		t.Errorf("expected pr_template_warning for title-only, got:\n%s", stderr.String())
	}
}

func TestPRTitleAndBody_AgentAuthored_NoBlankLine(t *testing.T) {
	path := withPRDescriptionPath(t)
	// Title and body separated only by a single \n (no blank line).
	if err := os.WriteFile(path, []byte("Title here\nBody starts immediately.\nMore body.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	title, body := prTitleAndBody(config{
		runID: "r", stageID: "s", backendURL: "https://x",
	}, "branch", &stderr)
	if title != "Title here" {
		t.Errorf("title = %q", title)
	}
	if !strings.Contains(body, "Body starts immediately.") {
		t.Errorf("body missing first line:\n%s", body)
	}
	// Should warn but still parse successfully.
	if !strings.Contains(stderr.String(), "pr_template_warning") {
		t.Errorf("expected pr_template_warning for missing blank line, got:\n%s", stderr.String())
	}
}

func TestPRTitleAndBody_FallbackWhenFileMissing(t *testing.T) {
	// withPRDescriptionPath points at a temp file, but we don't
	// write it. Helper must fall back silently.
	withPRDescriptionPath(t)
	var stderr strings.Builder
	title, body := prTitleAndBody(config{
		runID:      "11111111-2222-3333-4444-555555555555",
		stageID:    "22222222-3333-4444-5555-666666666666",
		backendURL: "https://api.fishhawk.test",
	}, "fishhawk/run-x/stage-y", &stderr)
	if !strings.HasPrefix(title, "Fishhawk: implement stage") {
		t.Errorf("fallback title should use the generic template, got %q", title)
	}
	if !strings.Contains(body, "Opened by Fishhawk for run") {
		t.Errorf("fallback body should use the generic template:\n%s", body)
	}
	// Missing file is the common no-op path; no warning logged.
	if strings.Contains(stderr.String(), "pr_template_") {
		t.Errorf("missing file should be silent, got:\n%s", stderr.String())
	}
}

func TestPRTitleAndBody_FallbackWhenFileEmpty(t *testing.T) {
	path := withPRDescriptionPath(t)
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	title, _ := prTitleAndBody(config{
		runID: "r", stageID: "s", backendURL: "https://x",
	}, "branch", &stderr)
	if !strings.HasPrefix(title, "Fishhawk:") {
		t.Errorf("empty file should fall back, got title %q", title)
	}
	if !strings.Contains(stderr.String(), "pr_template_invalid") {
		t.Errorf("empty file should produce pr_template_invalid log, got:\n%s", stderr.String())
	}
}

func TestPRTitleAndBody_FallbackWhenTitleEmpty(t *testing.T) {
	path := withPRDescriptionPath(t)
	// First line is whitespace; that's not a usable title.
	if err := os.WriteFile(path, []byte("   \n\nbody only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	title, _ := prTitleAndBody(config{
		runID: "r", stageID: "s", backendURL: "https://x",
	}, "branch", &stderr)
	if !strings.HasPrefix(title, "Fishhawk:") {
		t.Errorf("empty-title file should fall back, got title %q", title)
	}
	if !strings.Contains(stderr.String(), "pr_template_invalid") {
		t.Errorf("expected pr_template_invalid on empty title, got:\n%s", stderr.String())
	}
}

func TestRun_ImplementStage_PassesAgentAuthoredPRTitle(t *testing.T) {
	// End-to-end: agent wrote /tmp/fishhawk-pr.md (via the
	// PullRequestDescriptionPath constant in the prompt), the
	// runner reads it, and the agent's title + body land on the
	// gitops.OpenPRArgs and on the shipped pull_request artifact.
	prPath := withPRDescriptionPath(t)
	if err := os.WriteFile(prPath, []byte("Add make minio-init target\n\nThis adds an idempotent make target for the local MinIO bucket.\n\nCloses #184\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if fpr.gotArgs == nil {
		t.Fatal("OpenPR not called")
	}
	if fpr.gotArgs.Title != "Add make minio-init target" {
		t.Errorf("OpenPR title = %q, want agent-authored title", fpr.gotArgs.Title)
	}
	if !strings.Contains(fpr.gotArgs.Body, "Closes #184") {
		t.Errorf("OpenPR body should preserve agent's Closes line, got:\n%s", fpr.gotArgs.Body)
	}
	if !strings.Contains(fpr.gotArgs.Body, "Audit log:") {
		t.Errorf("OpenPR body should append Fishhawk attribution footer, got:\n%s", fpr.gotArgs.Body)
	}

	// And the shipped artifact body must reflect the same title.
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest not called")
	}
	var shipped map[string]any
	if err := json.Unmarshal(fu.gotPRArgs.Body, &shipped); err != nil {
		t.Fatal(err)
	}
	if got := shipped["title"]; got != "Add make minio-init target" {
		t.Errorf("shipped artifact title = %v, want agent-authored", got)
	}
}

// --- Local-runner mode (E22.8 / #406) ---

func TestRun_ImplementStage_NoPRFlag_SkipsPushAndOpen(t *testing.T) {
	// Local-runner mode: --no-pr makes the implement stage finish
	// after the trace upload. No push, no PR open, no
	// pull_request artifact shipped. Working tree stays dirty for
	// the operator to commit themselves.
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID: "22222222-3333-4444-5555-666666666666", StageType: "implement",
		Prompt: "implement", PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
		"--no-pr",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if fp.gotArgs != nil {
		t.Errorf("CommitAndPush should not be called under --no-pr; got %+v", fp.gotArgs)
	}
	if fpr.gotArgs != nil {
		t.Errorf("OpenPR should not be called under --no-pr; got %+v", fpr.gotArgs)
	}
	if fu.gotPRArgs != nil {
		t.Errorf("ShipPullRequest should not be called under --no-pr; got %+v", fu.gotPRArgs)
	}
	if !strings.Contains(stderr.String(), `"event":"implement_pr_skipped"`) {
		t.Errorf("missing implement_pr_skipped log line:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"reason":"no_pr_flag"`) {
		t.Errorf("skip line missing reason:\n%s", stderr.String())
	}
}

func TestRun_ImplementStage_GithubRepoFlag_OverridesEnv(t *testing.T) {
	// --github-repo flag takes precedence over GITHUB_REPOSITORY env.
	// Confirms the local-runner path (where the env isn't set) can
	// substitute via the flag without disturbing GHA mode (where
	// the env IS set; flag takes precedence when both present).
	implementEnv(t, "wrong/repo", "wrong-branch")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID: "22222222-3333-4444-5555-666666666666", StageType: "implement",
		Prompt: "implement", PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
		"--github-repo", "kuhlman-labs/fishhawk",
		"--base-branch", "main",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fpr.gotArgs == nil {
		t.Fatal("OpenPR not called")
	}
	if fpr.gotArgs.Owner != "kuhlman-labs" || fpr.gotArgs.Repo != "fishhawk" {
		t.Errorf("owner/repo = %q/%q, want flag-derived values", fpr.gotArgs.Owner, fpr.gotArgs.Repo)
	}
	if fpr.gotArgs.Base != "main" {
		t.Errorf("base = %q, want flag-derived 'main'", fpr.gotArgs.Base)
	}
}

func TestRun_ImplementStage_NoEnv_FlagsCarryEnoughInfo(t *testing.T) {
	// Local-runner posture: GITHUB_REPOSITORY / GITHUB_REF_NAME
	// are unset (operator workstation, not GHA). The flags alone
	// should be enough.
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_REF_NAME", "")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID: "22222222-3333-4444-5555-666666666666", StageType: "implement",
		Prompt: "implement", PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
		"--github-repo", "operator/scratch",
		"--base-branch", "develop",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fpr.gotArgs == nil {
		t.Fatal("OpenPR not called")
	}
	if fpr.gotArgs.Owner != "operator" || fpr.gotArgs.Repo != "scratch" {
		t.Errorf("owner/repo = %q/%q, want operator/scratch", fpr.gotArgs.Owner, fpr.gotArgs.Repo)
	}
	if fpr.gotArgs.Base != "develop" {
		t.Errorf("base = %q, want develop", fpr.gotArgs.Base)
	}
}

func TestParseFlags_LocalRunnerFields(t *testing.T) {
	// Direct test of parseFlags so the new fields are exercised
	// without spinning up the full agent loop.
	var out strings.Builder
	cfg, err := parseFlags([]string{
		"--run-id", "1",
		"--backend-url", "https://x",
		"--workflow", "w",
		"--stage", "s",
		"--github-repo", "owner/repo",
		"--base-branch", "release",
		"--no-pr",
	}, &out)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.githubRepo != "owner/repo" {
		t.Errorf("githubRepo = %q", cfg.githubRepo)
	}
	if cfg.baseBranch != "release" {
		t.Errorf("baseBranch = %q", cfg.baseBranch)
	}
	if !cfg.noPR {
		t.Errorf("noPR = %v, want true", cfg.noPR)
	}
}

func TestParseFlags_LocalRunnerFields_DefaultsEmpty(t *testing.T) {
	// Existing callers without the new flags get the GHA-equivalent
	// defaults: flag empty → fall back to env on the implement-stage
	// read path.
	var out strings.Builder
	cfg, err := parseFlags([]string{
		"--run-id", "1",
		"--backend-url", "https://x",
		"--workflow", "w",
		"--stage", "s",
	}, &out)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.githubRepo != "" || cfg.baseBranch != "" || cfg.noPR {
		t.Errorf("local-runner fields not zero-valued: %+v", cfg)
	}
}

func TestParseFlags_VerifyMaxIterations(t *testing.T) {
	// --verify-max-iterations parses into cfg; absent defaults to 0
	// (single-shot demote-on-failure gate).
	var out strings.Builder
	cfg, err := parseFlags([]string{
		"--run-id", "1",
		"--backend-url", "https://x",
		"--workflow", "w",
		"--stage", "s",
		"--verify-max-iterations", "4",
	}, &out)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.verifyMaxIterations != 4 {
		t.Errorf("verifyMaxIterations = %d, want 4", cfg.verifyMaxIterations)
	}

	cfgDefault, err := parseFlags([]string{
		"--run-id", "1",
		"--backend-url", "https://x",
		"--workflow", "w",
		"--stage", "s",
	}, &out)
	if err != nil {
		t.Fatalf("parseFlags (default): %v", err)
	}
	if cfgDefault.verifyMaxIterations != 0 {
		t.Errorf("verifyMaxIterations default = %d, want 0", cfgDefault.verifyMaxIterations)
	}
}

// TestRun_FetchPrompt_ServerTimeout_Applied verifies that when --timeout is
// not passed (default 0) and FetchPrompt returns AgentTimeoutSeconds=1800,
// the agent invocation's Budget.Timeout equals 30 minutes.
func TestRun_FetchPrompt_ServerTimeout_Applied(t *testing.T) {
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             "22222222-3333-4444-5555-666666666666",
		StageType:           "plan",
		Prompt:              "Hello agent.",
		PromptHash:          "deadbeef",
		AgentTimeoutSeconds: 1800, // 30 minutes
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		// --timeout intentionally absent (default 0)
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if invoker.gotInv == nil {
		t.Fatal("invoker.gotInv nil — invocation not captured")
	}
	if invoker.gotInv.Budget.Timeout != 30*time.Minute {
		t.Errorf("Budget.Timeout = %v, want 30m (from server AgentTimeoutSeconds=1800)",
			invoker.gotInv.Budget.Timeout)
	}
}

// withFakeRemoteBranchExists swaps the remoteBranchExists seam for the
// duration of a test. exists controls whether the fake reports the branch
// as present on the remote.
func withFakeRemoteBranchExists(t *testing.T, exists bool) {
	t.Helper()
	orig := remoteBranchExists
	remoteBranchExists = func(_ context.Context, _, _ string) bool { return exists }
	t.Cleanup(func() { remoteBranchExists = orig })
}

// TestRun_ImplementStage_DecomposedFirstChild verifies that when
// DecomposedFromRunID is set and the shared branch does not yet exist on the
// remote, the runner uses the shared branch name, pushes with --force-with-lease
// (ForceWithLease=true), does NOT rebase (RebaseFromRemote=false), and — per
// ADR-032 (#714) — does NOT open a PR (the parent run opens the consolidated PR).
func TestRun_ImplementStage_DecomposedFirstChild(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	withFakeRemoteBranchExists(t, false) // first child: branch not yet on remote

	parentRunID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             "22222222-3333-4444-5555-666666666666",
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		DecomposedFromRunID: parentRunID,
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	// shared branch: fishhawk/run-<shortParentID> — first 8 chars of parentRunID without hyphens
	wantBranch := "fishhawk/run-aaaaaaaa"
	if fp.gotArgs.Branch != wantBranch {
		t.Errorf("branch = %q, want %q (shared parent branch)", fp.gotArgs.Branch, wantBranch)
	}
	if !fp.gotArgs.ForceWithLease {
		t.Error("ForceWithLease = false, want true for decomposed child")
	}
	if fp.gotArgs.RebaseFromRemote {
		t.Error("RebaseFromRemote = true, want false for first child (branch not yet on remote)")
	}
	// First child cuts the shared branch from the freshly-fetched authoritative
	// base, not ambient HEAD, so a foreign #797 commit can't become the fork
	// point (ADR-035, #865). implementEnv sets the base ref to "main".
	if fp.gotArgs.FreshFetchBase != "main" {
		t.Errorf("FreshFetchBase = %q, want %q (first child cuts from freshly-fetched base, #865)", fp.gotArgs.FreshFetchBase, "main")
	}
	// Compile gate (#728) is now wired on the decomposed-child path too
	// (#766): a scope-bounded child commit is the highest-risk path for a
	// drift-dropped non-compiling HEAD, so it must be gated, not tolerated.
	if fp.gotArgs.VerifyCommit == nil {
		t.Error("VerifyCommit hook should be set for decomposed children (#766)")
	}
	// First child: per ADR-032 the child no longer opens a PR — the parent
	// run opens the single consolidated PR once all children settle.
	if fpr.gotArgs != nil {
		t.Error("OpenPR called for first decomposed child — should be suppressed (parent opens the PR)")
	}
	// But the child DOES report its push success via /pull-request with
	// Outcome=="pushed" (#771) so the backend can drive the child stage's
	// terminal transition its push_to_shared_branch trace gate left in
	// `running`. No PR artifact — just the shared-branch commit coordinates.
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (child-push report) not called for first decomposed child (#771)")
	}
	if fu.gotPRArgs.Outcome != "pushed" {
		t.Errorf("child-push report Outcome = %q, want \"pushed\"", fu.gotPRArgs.Outcome)
	}
	if fu.gotPRArgs.Branch != wantBranch {
		t.Errorf("child-push report Branch = %q, want %q", fu.gotPRArgs.Branch, wantBranch)
	}
	if fu.gotPRArgs.HeadSHA == "" || fu.gotPRArgs.BaseSHA == "" {
		t.Errorf("child-push report must carry head_sha + base_sha; got head=%q base=%q",
			fu.gotPRArgs.HeadSHA, fu.gotPRArgs.BaseSHA)
	}
}

// TestRun_ImplementStage_DecomposedSubsequentChild verifies that when
// DecomposedFromRunID is set and the shared branch already exists on the
// remote, the runner rebases (RebaseFromRemote=true), pushes with
// --force-with-lease, and skips OpenPR + ShipPullRequest.
func TestRun_ImplementStage_DecomposedSubsequentChild(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	withFakeRemoteBranchExists(t, true) // subsequent child: branch already on remote

	parentRunID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             "22222222-3333-4444-5555-666666666666",
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		DecomposedFromRunID: parentRunID,
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	wantBranch := "fishhawk/run-aaaaaaaa"
	if fp.gotArgs.Branch != wantBranch {
		t.Errorf("branch = %q, want %q (shared parent branch)", fp.gotArgs.Branch, wantBranch)
	}
	if !fp.gotArgs.ForceWithLease {
		t.Error("ForceWithLease = false, want true for decomposed child")
	}
	if !fp.gotArgs.RebaseFromRemote {
		t.Error("RebaseFromRemote = false, want true for subsequent child (branch exists on remote)")
	}
	// Subsequent child must leave FreshFetchBase empty: RebaseFromRemote owns
	// the routing for the existing shared branch, and the #865 fresh-fetch is
	// only for the first child's branch creation — no regression here.
	if fp.gotArgs.FreshFetchBase != "" {
		t.Errorf("FreshFetchBase = %q, want empty for subsequent child (RebaseFromRemote owns routing, #865)", fp.gotArgs.FreshFetchBase)
	}
	// (#766): the compile gate is wired on EVERY decomposed-child commit
	// path, not just the first child. Asserting it here too closes the gap
	// where a guard re-introduced as `isDecomposed && isSubsequent` would
	// leave the gate off for subsequent children while DecomposedFirstChild
	// still passed.
	if fp.gotArgs.VerifyCommit == nil {
		t.Error("VerifyCommit hook should be set for subsequent decomposed children (#766)")
	}
	// Subsequent child: PR must NOT be opened (first child already did it).
	if fpr.gotArgs != nil {
		t.Error("OpenPR called for subsequent decomposed child — should be skipped")
	}
	// A subsequent child still reports its push success (#771) — the gate is
	// per-child, not first-child-only.
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (child-push report) not called for subsequent decomposed child (#771)")
	}
	if fu.gotPRArgs.Outcome != "pushed" {
		t.Errorf("child-push report Outcome = %q, want \"pushed\"", fu.gotPRArgs.Outcome)
	}
	if fu.gotPRArgs.Branch != wantBranch {
		t.Errorf("child-push report Branch = %q, want %q", fu.gotPRArgs.Branch, wantBranch)
	}
}

// TestRun_ImplementStage_PushToSharedBranch_ManifestMatrix asserts the three
// forward-gate manifest flags (push_and_open_pr #742, push_to_shared_branch
// #771, push_fixup #794) and their MUTUAL EXCLUSIVITY across the {standalone,
// decomposed child, fix-up, --no-pr} stage matrix. A decomposed child stamps
// push_to_shared_branch; a standalone stamps push_and_open_pr; a fix-up stamps
// push_fixup; a --no-pr local run stamps none (it transitions on the trace
// upload as today). EXACTLY ONE of the three may be set per stage kind — no
// double-gating (CONDITION 5 of #794).
func TestRun_ImplementStage_PushToSharedBranch_ManifestMatrix(t *testing.T) {
	const parentRunID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	cases := []struct {
		name           string
		decomposedFrom string
		fixup          bool
		fixupBranch    string
		noPR           bool
		wantOpenPR     bool
		wantPushChild  bool
		wantPushFixup  bool
	}{
		{name: "standalone", wantOpenPR: true, wantPushChild: false, wantPushFixup: false},
		{name: "decomposed_child", decomposedFrom: parentRunID, wantOpenPR: false, wantPushChild: true, wantPushFixup: false},
		{name: "fixup", fixup: true, fixupBranch: "fishhawk/run-11111111/stage-22222222", wantOpenPR: false, wantPushChild: false, wantPushFixup: true},
		{name: "no_pr", noPR: true, wantOpenPR: false, wantPushChild: false, wantPushFixup: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			implementEnv(t, "kuhlman-labs/fishhawk", "main")
			withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
			withFakeRemoteBranchExists(t, false)
			fu := newFakeUploader(t)
			fu.promptResp = &upload.FetchedPrompt{
				StageID:             "22222222-3333-4444-5555-666666666666",
				StageType:           "implement",
				Prompt:              "implement",
				PromptHash:          "h",
				DecomposedFromRunID: tc.decomposedFrom,
				Fixup:               tc.fixup,
				FixupBranch:         tc.fixupBranch,
			}
			withFakeUploader(t, fu)
			withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

			args := []string{
				"--run-id", "11111111-2222-3333-4444-555555555555",
				"--backend-url", "https://api.fishhawk.test",
				"--workflow", "feature_change", "--stage", "implement",
				"--stage-id", "22222222-3333-4444-5555-666666666666",
				"--fetch-prompt", "--upload-trace",
			}
			if tc.noPR {
				args = append(args, "--no-pr")
			}
			var stderr strings.Builder
			if got := run(args, &stderr); got != exitOK {
				t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
			}
			if len(fu.gotShipCalls) == 0 {
				t.Fatal("no trace uploaded")
			}
			manifest, _, _, err := openBundleForTest(fu.gotShipCalls[0].Bundle)
			if err != nil {
				t.Fatalf("open bundle: %v", err)
			}
			if manifest.PushAndOpenPR != tc.wantOpenPR {
				t.Errorf("manifest.PushAndOpenPR = %v, want %v", manifest.PushAndOpenPR, tc.wantOpenPR)
			}
			if manifest.PushToSharedBranch != tc.wantPushChild {
				t.Errorf("manifest.PushToSharedBranch = %v, want %v", manifest.PushToSharedBranch, tc.wantPushChild)
			}
			if manifest.PushFixup != tc.wantPushFixup {
				t.Errorf("manifest.PushFixup = %v, want %v", manifest.PushFixup, tc.wantPushFixup)
			}
			// Exactly one of the three forward-gate flags may be set per stage
			// kind (no double-gating). --no-pr sets zero, which is also valid.
			set := 0
			for _, b := range []bool{manifest.PushAndOpenPR, manifest.PushToSharedBranch, manifest.PushFixup} {
				if b {
					set++
				}
			}
			if set > 1 {
				t.Errorf("forward-gate flags not mutually exclusive: push_and_open_pr=%v push_to_shared_branch=%v push_fixup=%v",
					manifest.PushAndOpenPR, manifest.PushToSharedBranch, manifest.PushFixup)
			}
		})
	}
}

// TestRun_ImplementStage_DecomposedChildPushFailure_ReportsFailed confirms
// the load-bearing #771 fix: when a decomposed child's commit/push onto the
// shared branch fails (a generic git/network error → category C), the runner
// reports the failure to /pull-request with Outcome=="failed" so the backend
// fails the child stage its push_to_shared_branch trace gate left in `running`
// — instead of leaving the trace-time succeeded zombie with no code on the
// shared branch.
func TestRun_ImplementStage_DecomposedChildPushFailure_ReportsFailed(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	withFakeRemoteBranchExists(t, false)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             "22222222-3333-4444-5555-666666666666",
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		DecomposedFromRunID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: errors.New("ssh-agent fetch failed")}
	withFakeGitOps(t, fp, &fakePROpener{})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after a child push failure (#771)")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "C" {
		t.Errorf("child-push failure report = {outcome:%q, category:%q}, want {failed, C}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
}

// TestRun_ImplementStage_DecomposedChildPushReportFailure_FailsStage covers the
// load-bearing reverse-zombie case (#771): the push onto the shared branch
// SUCCEEDS, but the Outcome=="pushed" report to /pull-request fails (e.g. a
// backend 5xx after the push landed). The stage must still end as a failure
// (exitFailure) — falling through to reportPullRequestFailure — NOT hang in the
// trace-gated `running` state, since the trace handler deferred the terminal
// transition pending a report.
func TestRun_ImplementStage_DecomposedChildPushReportFailure_FailsStage(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	withFakeRemoteBranchExists(t, false)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             "22222222-3333-4444-5555-666666666666",
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		DecomposedFromRunID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	// Push succeeds (fakePusher has no err); the child-push report fails.
	fu.shipErr = errors.New("backend 503 on child-push report")
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure: a child-push report failure must fail the stage (retryable), not leave it hung in running", got)
	}
}

// TestRun_ImplementStage_Fixup_CommitsToExistingBranch verifies the
// sub-plan C fix-up path (#762): when the prompt response marks the stage a
// fix-up pass carrying the existing PR branch, the runner commits onto THAT
// branch with RebaseFromRemote=true and does NOT open a new PR or ship a
// fresh pull_request artifact — the open PR's head advances via the push.
func TestRun_ImplementStage_Fixup_CommitsToExistingBranch(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	existingBranch := "fishhawk/run-11111111/stage-22222222"
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:     "22222222-3333-4444-5555-666666666666",
		StageType:   "implement",
		Prompt:      "implement",
		PromptHash:  "h",
		Fixup:       true,
		FixupBranch: existingBranch,
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if fp.gotArgs == nil {
		t.Fatal("CommitAndPush not called")
	}
	if fp.gotArgs.Branch != existingBranch {
		t.Errorf("branch = %q, want %q (existing PR branch)", fp.gotArgs.Branch, existingBranch)
	}
	if !fp.gotArgs.RebaseFromRemote {
		t.Error("RebaseFromRemote = false, want true (fix-up rebases the existing PR branch)")
	}
	// A fix-up updates the open PR; it must not open a new one or ship a
	// fresh pull_request artifact.
	if fpr.gotArgs != nil {
		t.Error("OpenPR called for fix-up pass — should be skipped (PR already exists)")
	}
	// A fix-up DOES report push success via a {outcome:"fixup_pushed"}
	// /pull-request report (#794) so the backend drives the terminal
	// transition its push_fixup trace gate deferred — it must NOT ship a fresh
	// PR artifact (empty Outcome with a Body).
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (fixup_pushed report) must be called after a fix-up push (#794)")
	}
	if fu.gotPRArgs.Outcome != "fixup_pushed" {
		t.Errorf("fix-up report outcome = %q, want %q", fu.gotPRArgs.Outcome, "fixup_pushed")
	}
	if fu.gotPRArgs.Branch != existingBranch {
		t.Errorf("fix-up report branch = %q, want %q", fu.gotPRArgs.Branch, existingBranch)
	}
	if len(fu.gotPRArgs.Body) != 0 {
		t.Errorf("fix-up report must not carry a PR artifact body, got %d bytes", len(fu.gotPRArgs.Body))
	}
	if !strings.Contains(stderr.String(), `"event":"implement_fixup_pushed"`) {
		t.Errorf("missing implement_fixup_pushed log line:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"implement_fixup_push_reported"`) {
		t.Errorf("missing implement_fixup_push_reported log line:\n%s", stderr.String())
	}
}

// fixupPromptResp builds the fix-up prompt response the #967 base-
// establishment tests dispatch against.
func fixupPromptResp(expectedHeadSHA string) *upload.FetchedPrompt {
	return &upload.FetchedPrompt{
		StageID:              "22222222-3333-4444-5555-666666666666",
		StageType:            "implement",
		Prompt:               "implement",
		PromptHash:           "h",
		Fixup:                true,
		FixupBranch:          "fishhawk/run-11111111/stage-22222222",
		FixupExpectedHeadSHA: expectedHeadSHA,
	}
}

// runFixupStage drives run() with the standard fix-up argv.
func runFixupStage(t *testing.T, stderr *strings.Builder) int {
	t.Helper()
	return run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, stderr)
}

// TestRun_Fixup_EstablishesBaseBeforeAgentInvoke is the #967 regression: a
// fix-up dispatch with the operator's tree on a DIFFERENT ref (main) must
// fetch + checkout the run's PR branch BEFORE the agent is invoked — never
// run the agent against the operator's incidental checkout — and restore
// the operator's original ref afterwards. The success path fires BOTH
// restore defers (run()-level #967 + openPRAndShipArtifact's #911); the
// second is a harmless same-ref restore, so every call targets the
// original ref and no restore-failure event is emitted.
func TestRun_Fixup_EstablishesBaseBeforeAgentInvoke(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")

	// Ordered spy: the agent invocation must observe the checkout already
	// requested.
	var order []string
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(_ int, _ agent.Invocation) {
			order = append(order, "invoke")
		},
	})
	fu := newFakeUploader(t)
	fu.promptResp = fixupPromptResp("fixup-branch-tip-sha")
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	withFakeGitOps(t, fp, &fakePROpener{})

	// The operator's tree sits on main — the exact #967 shape. Both restore
	// seams record into restoredRefs: run()'s fixup defer calls restoreHead,
	// openPRAndShipArtifact's defer calls restoreHeadPreserving (#943).
	var restoredRefs []string
	origCap, origRes, origCheckout := captureHead, restoreHead, checkoutFixupBase
	origResPres := restoreHeadPreserving
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		return "main", false, nil
	}
	restoreHead = func(_ context.Context, _, ref string) error {
		restoredRefs = append(restoredRefs, ref)
		return nil
	}
	restoreHeadPreserving = func(_ context.Context, _, ref string, _ []string) error {
		restoredRefs = append(restoredRefs, ref)
		return nil
	}
	var gotBranch, gotRemote string
	checkoutFixupBase = func(_ context.Context, _, remote, branch string) (string, error) {
		order = append(order, "checkout")
		gotRemote = remote
		gotBranch = branch
		return "fixup-branch-tip-sha", nil
	}
	t.Cleanup(func() {
		captureHead = origCap
		restoreHead = origRes
		checkoutFixupBase = origCheckout
		restoreHeadPreserving = origResPres
	})

	var stderr strings.Builder
	if got := runFixupStage(t, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	if len(order) < 2 || order[0] != "checkout" || order[1] != "invoke" {
		t.Errorf("order = %v, want the run-branch checkout BEFORE the agent invocation", order)
	}
	if gotBranch != "fishhawk/run-11111111/stage-22222222" {
		t.Errorf("checkout branch = %q, want the prompt's fixup_branch", gotBranch)
	}
	if gotRemote != gitops.DefaultRemote {
		t.Errorf("checkout remote = %q, want %q", gotRemote, gitops.DefaultRemote)
	}
	// Binding amendment (opus, step 6a): the success path emits
	// fixup_base_established carrying branch + head_sha + original_ref.
	for _, want := range []string{
		`"event":"fixup_base_established"`,
		`"branch":"fishhawk/run-11111111/stage-22222222"`,
		`"head_sha":"fixup-branch-tip-sha"`,
		`"original_ref":"main"`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("missing %s in fixup_base_established emission:\n%s", want, stderr.String())
		}
	}
	// Both restore defers fired; every restore targets the ORIGINAL ref and
	// the double restore is error-free.
	if len(restoredRefs) != 2 {
		t.Errorf("restoreHead called %d times, want 2 (run()-level + openPRAndShipArtifact defers)", len(restoredRefs))
	}
	for i, ref := range restoredRefs {
		if ref != "main" {
			t.Errorf("restoredRefs[%d] = %q, want %q (the operator's original ref)", i, ref, "main")
		}
	}
	if strings.Contains(stderr.String(), `"event":"working_tree_restore_failed"`) {
		t.Errorf("double restore emitted a restore-failure event:\n%s", stderr.String())
	}
}

// TestRun_Fixup_BaseMismatch_FailsBeforeAgentInvoke: when the fetched
// branch tip differs from the backend-advertised fixup_expected_head_sha,
// the runner fails fast — the agent is NEVER invoked, the mismatch event
// names both SHAs (ADR-035), and the operator's original ref is restored
// (the checkout had already moved HEAD).
func TestRun_Fixup_BaseMismatch_FailsBeforeAgentInvoke(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	inv := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, inv)
	fu := newFakeUploader(t)
	fu.promptResp = fixupPromptResp("expected-recorded-head-sha")
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	var restoredRefs []string
	origCap, origRes, origCheckout := captureHead, restoreHead, checkoutFixupBase
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		return "main", false, nil
	}
	restoreHead = func(_ context.Context, _, ref string) error {
		restoredRefs = append(restoredRefs, ref)
		return nil
	}
	checkoutFixupBase = func(_ context.Context, _, _, _ string) (string, error) {
		return "foreign-tip-sha", nil
	}
	t.Cleanup(func() { captureHead = origCap; restoreHead = origRes; checkoutFixupBase = origCheckout })

	var stderr strings.Builder
	if got := runFixupStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure on a lineage mismatch:\n%s", got, stderr.String())
	}
	if inv.gotInv != nil {
		t.Error("agent was invoked despite the fixup base mismatch — must fail fast BEFORE any invocation")
	}
	for _, want := range []string{
		`"event":"fixup_base_mismatch"`,
		`"fetched_tip_sha":"foreign-tip-sha"`,
		`"expected_head_sha":"expected-recorded-head-sha"`,
		`"reason":"fixup_base_mismatch"`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("missing %s in mismatch emission:\n%s", want, stderr.String())
		}
	}
	// The checkout moved HEAD before the comparison, so the run()-level
	// defer must put the operator back.
	if len(restoredRefs) != 1 || restoredRefs[0] != "main" {
		t.Errorf("restoredRefs = %v, want exactly one restore to %q", restoredRefs, "main")
	}
}

// TestRun_Fixup_MissingExpectedSHA_ProceedsWithCheckoutOnly: an empty
// fixup_expected_head_sha (older backend / backend-side resolution
// failure) must not block the pass — the runner checks the branch out,
// emits the skip warning, and invokes the agent.
func TestRun_Fixup_MissingExpectedSHA_ProceedsWithCheckoutOnly(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	inv := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, inv)
	fu := newFakeUploader(t)
	fu.promptResp = fixupPromptResp("")
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	var stderr strings.Builder
	if got := runFixupStage(t, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if inv.gotInv == nil {
		t.Error("agent was not invoked — a missing expected SHA must proceed, not block")
	}
	if !strings.Contains(stderr.String(), `"event":"fixup_expected_head_missing"`) {
		t.Errorf("missing fixup_expected_head_missing warning:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"fixup_base_established"`) {
		t.Errorf("missing fixup_base_established event:\n%s", stderr.String())
	}
}

// TestRun_Fixup_AgentFailureAfterCheckout_RestoresOriginalRef: an agent
// (category-A) failure after the fix-up checkout never reaches
// openPRAndShipArtifact's restore defer — the run()-level defer (#967)
// must still return the operator to their original ref.
func TestRun_Fixup_AgentFailureAfterCheckout_RestoresOriginalRef(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: errors.New("agent crash"),
	})
	fu := newFakeUploader(t)
	fu.promptResp = fixupPromptResp("fixup-branch-tip-sha")
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	var restoredRefs []string
	origCap, origRes := captureHead, restoreHead
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		return "main", false, nil
	}
	restoreHead = func(_ context.Context, _, ref string) error {
		restoredRefs = append(restoredRefs, ref)
		return nil
	}
	t.Cleanup(func() { captureHead = origCap; restoreHead = origRes })

	var stderr strings.Builder
	if got := runFixupStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure on agent crash:\n%s", got, stderr.String())
	}
	if len(restoredRefs) != 1 || restoredRefs[0] != "main" {
		t.Errorf("restoredRefs = %v, want exactly one restore to %q (run()-level defer on the agent-failure path)",
			restoredRefs, "main")
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("missing working_tree_restored event on the agent-failure path:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_RestoresOperatorBranch_OnSuccess is the #911
// defer-wiring seam (success half): after a SUCCESSFUL implement push, the
// runner must return the operator's checkout to the branch CaptureHead
// recorded — restoreHead is invoked exactly once with the captured ref, and a
// working_tree_restored event is emitted. The real branch-switch semantics
// (force off a dirty run branch, leaving a clean tree + reachable commit) are
// proven by the gitops RestoreHead unit test; this test pins the wiring in
// openPRAndShipArtifact so a regression that drops the defer is caught.
func TestRun_ImplementStage_RestoresOperatorBranch_OnSuccess(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	gotRef, restoreCalls := installRestoreSpy(t, "operator-main")

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if *restoreCalls != 1 {
		t.Errorf("restoreHead called %d times, want 1 (defer must fire on the success path)", *restoreCalls)
	}
	if *gotRef != "operator-main" {
		t.Errorf("restoreHead ref = %q, want %q (the captured original branch)", *gotRef, "operator-main")
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("missing working_tree_restored log line:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_RestoresOperatorBranch_OnCommitPushFailure is the
// #911 defer-wiring seam (failure half): when CommitAndPush fails AFTER
// switching HEAD onto the run branch — e.g. the #800/#908 committed-test
// verify gate flaking — the runner must STILL return the operator to their
// original branch. The defer runs on the error return path too, so restoreHead
// is invoked once with the captured ref even though the stage fails. This is
// the exact #911 leftover: a failed fix-up/verify gate stranding the operator
// on a dirty run branch.
func TestRun_ImplementStage_RestoresOperatorBranch_OnCommitPushFailure(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	// Model the in-CommitAndPush verify-gate failure (#800) that leaves HEAD on
	// the run branch — the #911 trigger.
	fp := &fakePusher{err: gitops.ErrCommittedTestsFailed}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	gotRef, restoreCalls := installRestoreSpy(t, "operator-main")

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure (commit+push gate failed):\n%s", got, stderr.String())
	}
	if *restoreCalls != 1 {
		t.Errorf("restoreHead called %d times, want 1 (defer must fire on the failure path too)", *restoreCalls)
	}
	if *gotRef != "operator-main" {
		t.Errorf("restoreHead ref = %q, want %q (the captured original branch)", *gotRef, "operator-main")
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("missing working_tree_restored log line on the failure path:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_CaptureFailure_DoesNotBreakPush is the #911
// capture-failure path: when captureHead returns an error the defer is NOT
// installed (no original ref to restore to), but a capture failure must never
// break the push — the stage still succeeds, restoreHead is never called, and a
// working_tree_capture_failed event is emitted instead of working_tree_restored.
func TestRun_ImplementStage_CaptureFailure_DoesNotBreakPush(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	// captureHead fails; neither restore seam must be reached because the
	// defers are skipped. Install AFTER withFakeGitOps so these override its
	// no-op stubs.
	restoreCalls := 0
	origCap, origRes, origResPres := captureHead, restoreHead, restoreHeadPreserving
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		return "", false, errors.New("symbolic-ref failed")
	}
	restoreHead = func(_ context.Context, _, _ string) error { restoreCalls++; return nil }
	restoreHeadPreserving = func(_ context.Context, _, _ string, _ []string) error { restoreCalls++; return nil }
	t.Cleanup(func() { captureHead = origCap; restoreHead = origRes; restoreHeadPreserving = origResPres })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (a capture failure must not break the push):\n%s", got, stderr.String())
	}
	if restoreCalls != 0 {
		t.Errorf("restoreHead called %d times, want 0 (no defer installed after a capture failure)", restoreCalls)
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_capture_failed"`) {
		t.Errorf("missing working_tree_capture_failed log line:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("working_tree_restored must NOT be emitted when capture failed:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_RestoreFailure_PreservesOutcome is the #911
// restore-failure path: when restoreHead itself errors, the defer must emit a
// working_tree_restore_failed event (not working_tree_restored) and MUST NOT
// override the function's primary success outcome — restore is best-effort and
// log-only.
func TestRun_ImplementStage_RestoreFailure_PreservesOutcome(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	// captureHead succeeds (defer installs); the restore itself fails — the
	// openPRAndShipArtifact defer calls restoreHeadPreserving (#943). Install
	// AFTER withFakeGitOps so these override its no-op stubs.
	origCap, origResPres := captureHead, restoreHeadPreserving
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		return "operator-main", false, nil
	}
	restoreHeadPreserving = func(_ context.Context, _, _ string, _ []string) error {
		return errors.New("checkout --force failed")
	}
	t.Cleanup(func() { captureHead = origCap; restoreHeadPreserving = origResPres })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (a restore failure must not override the primary push outcome):\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_restore_failed"`) {
		t.Errorf("missing working_tree_restore_failed log line:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("working_tree_restored must NOT be emitted when restore failed:\n%s", stderr.String())
	}
}

// driftPartitionArgv drives run() with the standard implement argv for the
// #943 drift-partition tests. The caller installs the invoker/uploader/git
// fakes and seam spies first.
func driftPartitionArgv(t *testing.T, stderr *strings.Builder) int {
	t.Helper()
	return run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, stderr)
}

// implementPromptResp is the minimal implement-stage prompt response the
// #943 tests fetch.
func implementPromptResp() *upload.FetchedPrompt {
	return &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
}

// TestRun_ImplementStage_DriftPartition_CleansAgentPreservesOperator is the
// #943 partition wiring: CommitAndPush's ScopeDrift is split against the
// pre-agent dirty snapshot — paths NOT dirty pre-agent are agent-introduced
// and go to cleanDriftPaths, paths dirty pre-agent are operator-owned and go
// to restoreHeadPreserving's preserve set — and the drift_cleaned /
// drift_preserved events land in the log sink.
func TestRun_ImplementStage_DriftPartition_CleansAgentPreservesOperator(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = implementPromptResp()
	withFakeUploader(t, fu)
	fp := &fakePusher{result: &gitops.CommitAndPushResult{
		HeadSHA:    "head-sha-abc",
		BaseSHA:    "base-sha-def",
		ScopeDrift: []string{"operator-edit.txt", "agent-drift.txt", "agent-new.txt"},
	}}
	withFakeGitOps(t, fp, &fakePROpener{})

	// Spies installed AFTER withFakeGitOps override its no-op stubs.
	var cleanedPaths, preservedPaths []string
	origDirty, origClean, origResPres := dirtyPaths, cleanDriftPaths, restoreHeadPreserving
	dirtyPaths = func(_ context.Context, _ string) ([]string, error) {
		return []string{"operator-edit.txt"}, nil
	}
	cleanDriftPaths = func(_ context.Context, _ string, paths []string) error {
		cleanedPaths = append(cleanedPaths, paths...)
		return nil
	}
	restoreHeadPreserving = func(_ context.Context, _, _ string, preserve []string) error {
		preservedPaths = append(preservedPaths, preserve...)
		return nil
	}
	t.Cleanup(func() { dirtyPaths = origDirty; cleanDriftPaths = origClean; restoreHeadPreserving = origResPres })

	var stderr strings.Builder
	if got := driftPartitionArgv(t, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if len(cleanedPaths) != 2 || cleanedPaths[0] != "agent-drift.txt" || cleanedPaths[1] != "agent-new.txt" {
		t.Errorf("cleanDriftPaths got %v, want the agent-introduced drift [agent-drift.txt agent-new.txt]", cleanedPaths)
	}
	if len(preservedPaths) != 1 || preservedPaths[0] != "operator-edit.txt" {
		t.Errorf("restoreHeadPreserving preserve = %v, want the operator's pre-agent edit [operator-edit.txt]", preservedPaths)
	}
	for _, want := range []string{
		`"event":"drift_cleaned"`,
		`"paths":["agent-drift.txt","agent-new.txt"]`,
		`"event":"drift_preserved"`,
		`"paths":["operator-edit.txt"]`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("missing %s in drift-partition emission:\n%s", want, stderr.String())
		}
	}
}

// TestRun_ImplementStage_DirtyCaptureFailure_DisablesCleanup: when the
// pre-agent dirty snapshot fails there is no trustworthy baseline, so the
// runner must never revert blind — cleanup is disabled for the stage
// (cleanDriftPaths never called, no drift events), the diagnostic
// working_tree_dirty_capture_failed is emitted, and the push outcome is
// unaffected.
func TestRun_ImplementStage_DirtyCaptureFailure_DisablesCleanup(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = implementPromptResp()
	withFakeUploader(t, fu)
	fp := &fakePusher{result: &gitops.CommitAndPushResult{
		HeadSHA:    "head-sha-abc",
		BaseSHA:    "base-sha-def",
		ScopeDrift: []string{"agent-drift.txt"},
	}}
	withFakeGitOps(t, fp, &fakePROpener{})

	cleanCalls := 0
	var preservedPaths []string
	origDirty, origClean, origResPres := dirtyPaths, cleanDriftPaths, restoreHeadPreserving
	dirtyPaths = func(_ context.Context, _ string) ([]string, error) {
		return nil, errors.New("status failed")
	}
	cleanDriftPaths = func(_ context.Context, _ string, _ []string) error { cleanCalls++; return nil }
	restoreHeadPreserving = func(_ context.Context, _, _ string, preserve []string) error {
		preservedPaths = append(preservedPaths, preserve...)
		return nil
	}
	t.Cleanup(func() { dirtyPaths = origDirty; cleanDriftPaths = origClean; restoreHeadPreserving = origResPres })

	var stderr strings.Builder
	if got := driftPartitionArgv(t, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (a dirty-capture failure must not break the push):\n%s", got, stderr.String())
	}
	if cleanCalls != 0 {
		t.Errorf("cleanDriftPaths called %d times, want 0 — never revert blind without a pre-agent baseline", cleanCalls)
	}
	if len(preservedPaths) != 0 {
		t.Errorf("restoreHeadPreserving preserve = %v, want empty (plain restore semantics)", preservedPaths)
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_dirty_capture_failed"`) {
		t.Errorf("missing working_tree_dirty_capture_failed diagnostic:\n%s", stderr.String())
	}
	for _, banned := range []string{`"event":"drift_cleaned"`, `"event":"drift_preserved"`} {
		if strings.Contains(stderr.String(), banned) {
			t.Errorf("unexpected %s after a dirty-capture failure:\n%s", banned, stderr.String())
		}
	}
}

// TestRun_ImplementStage_NoChangesWithDrift_StillCleans: the
// NoChanges-with-drift return (everything the agent touched was out of scope
// — no branch created, nothing pushed) still reports ScopeDrift, and the
// agent-introduced drift must still be cleaned so it doesn't accumulate
// across loop runs.
func TestRun_ImplementStage_NoChangesWithDrift_StillCleans(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = implementPromptResp()
	withFakeUploader(t, fu)
	fp := &fakePusher{result: &gitops.CommitAndPushResult{
		NoChanges:  true,
		BaseSHA:    "base-sha-def",
		ScopeDrift: []string{"agent-drift.txt"},
	}}
	withFakeGitOps(t, fp, &fakePROpener{})

	var cleanedPaths []string
	origDirty, origClean := dirtyPaths, cleanDriftPaths
	dirtyPaths = func(_ context.Context, _ string) ([]string, error) { return nil, nil }
	cleanDriftPaths = func(_ context.Context, _ string, paths []string) error {
		cleanedPaths = append(cleanedPaths, paths...)
		return nil
	}
	t.Cleanup(func() { dirtyPaths = origDirty; cleanDriftPaths = origClean })

	var stderr strings.Builder
	if got := driftPartitionArgv(t, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if len(cleanedPaths) != 1 || cleanedPaths[0] != "agent-drift.txt" {
		t.Errorf("cleanDriftPaths got %v, want [agent-drift.txt] on the NoChanges-with-drift path", cleanedPaths)
	}
	if !strings.Contains(stderr.String(), `"event":"drift_cleaned"`) {
		t.Errorf("missing drift_cleaned event:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"implement_no_changes"`) {
		t.Errorf("missing implement_no_changes event:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_DriftCleanFailure_PreservesOutcome: a
// cleanDriftPaths failure is best-effort and log-only — drift_clean_failed is
// emitted with the paths and detail, and the stage's primary push outcome
// stands.
func TestRun_ImplementStage_DriftCleanFailure_PreservesOutcome(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = implementPromptResp()
	withFakeUploader(t, fu)
	fp := &fakePusher{result: &gitops.CommitAndPushResult{
		HeadSHA:    "head-sha-abc",
		BaseSHA:    "base-sha-def",
		ScopeDrift: []string{"agent-drift.txt"},
	}}
	withFakeGitOps(t, fp, &fakePROpener{})

	origDirty, origClean := dirtyPaths, cleanDriftPaths
	dirtyPaths = func(_ context.Context, _ string) ([]string, error) { return nil, nil }
	cleanDriftPaths = func(_ context.Context, _ string, _ []string) error {
		return errors.New("stash push failed")
	}
	t.Cleanup(func() { dirtyPaths = origDirty; cleanDriftPaths = origClean })

	var stderr strings.Builder
	if got := driftPartitionArgv(t, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (drift cleanup is log-only; it must never override the push outcome):\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"drift_clean_failed"`) {
		t.Errorf("missing drift_clean_failed event:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"detail":"stash push failed"`) {
		t.Errorf("missing failure detail in drift_clean_failed:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), `"event":"drift_cleaned"`) {
		t.Errorf("drift_cleaned must NOT be emitted when cleaning failed:\n%s", stderr.String())
	}
}

// installRestoreSpy swaps the #911 capture/restore seam for recording spies:
// captureHead returns origRef, and both restore seams — restoreHead (run()'s
// nets) and restoreHeadPreserving (openPRAndShipArtifact's defer, #943) —
// record the ref they were called with into a shared call count. Returns
// pointers the caller asserts on. Must be called AFTER withFakeGitOps (whose
// no-op stubs it overrides). Restored via t.Cleanup.
func installRestoreSpy(t *testing.T, origRef string) (gotRef *string, calls *int) {
	t.Helper()
	gotRef = new(string)
	calls = new(int)
	origCap, origRes, origResPres := captureHead, restoreHead, restoreHeadPreserving
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		return origRef, false, nil
	}
	restoreHead = func(_ context.Context, _, ref string) error {
		*gotRef = ref
		*calls++
		return nil
	}
	restoreHeadPreserving = func(_ context.Context, _, ref string, _ []string) error {
		*gotRef = ref
		*calls++
		return nil
	}
	t.Cleanup(func() { captureHead = origCap; restoreHead = origRes; restoreHeadPreserving = origResPres })
	return gotRef, calls
}

// TestRun_ImplementStage_CapturesHeadBeforeAgentInvoke is the #941 fix: the
// restore target must be the ref the operator started on, captured BEFORE the
// agent is invoked — not HEAD re-read at upload time, after an agent that ran
// `git checkout -b` mid-stage could have moved HEAD onto its own branch. The
// fake captureHead returns "operator-main" until the invoker runs, then
// "agent-branch"; the fake invoker's onInvoke flips the sentinel. Because the
// real capture now happens before invocation, restoreHead must receive
// "operator-main" — the agent-moved ref can never become the restore target.
// Under the old capture-at-upload ordering captureHead would have run after the
// invoke and returned "agent-branch", failing this assertion.
func TestRun_ImplementStage_CapturesHeadBeforeAgentInvoke(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")

	// Shared sentinel: false until the agent has been invoked. captureHead
	// reads it to decide which ref the operator's HEAD "points at".
	agentInvoked := false
	withFakeInvoker(t, &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(_ int, _ agent.Invocation) {
			// The agent moves HEAD onto its own branch mid-stage.
			agentInvoked = true
		},
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	// captureHead returns the pre-agent ref until the invoker runs, then the
	// agent's branch. Install AFTER withFakeGitOps so these override its no-op
	// stubs. Both restore seams record the ref they were handed — the
	// openPRAndShipArtifact defer calls restoreHeadPreserving (#943).
	gotRef := new(string)
	origCap, origRes, origResPres := captureHead, restoreHead, restoreHeadPreserving
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		if agentInvoked {
			return "agent-branch", false, nil
		}
		return "operator-main", false, nil
	}
	restoreHead = func(_ context.Context, _, ref string) error {
		*gotRef = ref
		return nil
	}
	restoreHeadPreserving = func(_ context.Context, _, ref string, _ []string) error {
		*gotRef = ref
		return nil
	}
	t.Cleanup(func() { captureHead = origCap; restoreHead = origRes; restoreHeadPreserving = origResPres })

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if *gotRef != "operator-main" {
		t.Errorf("restoreHead ref = %q, want %q (HEAD must be captured BEFORE the agent invoke, so an agent-moved branch can't become the restore target)", *gotRef, "operator-main")
	}
}

// runFailedImplementStage drives run() with the standard implement argv for
// the #953 failure-path tests. The caller installs the invoker/uploader/git
// fakes first.
func runFailedImplementStage(t *testing.T, stderr *strings.Builder, extraArgs ...string) int {
	t.Helper()
	args := []string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}
	return run(append(args, extraArgs...), stderr)
}

// installMovingHeadSpy swaps the capture/restore seam for the #953 tests: the
// FIRST captureHead call (run()'s pre-agent capture) returns origRef; every
// subsequent call (the run()-level defer's re-read) returns movedRef —
// modeling an agent that ran `git checkout -b` mid-stage. restoreHead records
// the refs it was handed. Must be called AFTER withFakeGitOps. Restored via
// t.Cleanup.
func installMovingHeadSpy(t *testing.T, origRef, movedRef string) (restoredRefs *[]string) {
	t.Helper()
	restoredRefs = new([]string)
	captureCalls := 0
	origCap, origRes := captureHead, restoreHead
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		captureCalls++
		if captureCalls == 1 {
			return origRef, false, nil
		}
		return movedRef, false, nil
	}
	restoreHead = func(_ context.Context, _, ref string) error {
		*restoredRefs = append(*restoredRefs, ref)
		return nil
	}
	t.Cleanup(func() { captureHead = origCap; restoreHead = origRes })
	return restoredRefs
}

// TestRun_ImplementStage_AgentFailure_MovedHead_RestoresOperatorRef is the
// #953 fix (the #941 residual): an implement stage whose agent moves HEAD
// (e.g. `git checkout -b`) and then FAILS never reaches openPRAndShipArtifact's
// restore defer — the run()-level net must still return the operator to their
// original ref, and the stage's failure exit code is preserved.
func TestRun_ImplementStage_AgentFailure_MovedHead_RestoresOperatorRef(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: errors.New("agent crash"),
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	restoredRefs := installMovingHeadSpy(t, "operator-main", "agent-branch")

	var stderr strings.Builder
	if got := runFailedImplementStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure on agent crash:\n%s", got, stderr.String())
	}
	if len(*restoredRefs) != 1 || (*restoredRefs)[0] != "operator-main" {
		t.Errorf("restoredRefs = %v, want exactly one restore to %q (run()-level net on the agent-failure path)",
			*restoredRefs, "operator-main")
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("missing working_tree_restored event on the agent-failure path:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_AgentFailure_UnmovedHead_SkipsRestore pins the #953
// destructive-checkout guard: when the agent FAILED but never moved HEAD (the
// common case — it only edited files), the run()-level net must NOT call
// restoreHead. restoreHead is `git checkout --force`, which would discard the
// staged+unstaged tracked edits the operator inspects after a failure. This
// test fails if the moved-HEAD guard is removed.
func TestRun_ImplementStage_AgentFailure_UnmovedHead_SkipsRestore(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: errors.New("agent crash"),
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	// HEAD never moves: every capture (pre-agent + in-defer re-read) reports
	// the same ref.
	restoredRefs := installMovingHeadSpy(t, "operator-main", "operator-main")

	var stderr strings.Builder
	if got := runFailedImplementStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure on agent crash:\n%s", got, stderr.String())
	}
	if len(*restoredRefs) != 0 {
		t.Errorf("restoredRefs = %v, want none — an unmoved HEAD must never be force-checked-out (the dirty tree is the operator's to inspect)",
			*restoredRefs)
	}
	if strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("working_tree_restored must NOT be emitted when HEAD never moved:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_AgentFailure_NoPR_SkipsRestore pins the #953 --no-pr
// opt-out: local-runner mode deliberately leaves the tree as-is (the dirty
// tree IS the deliverable), so the run()-level net is skipped entirely — even
// when the agent moved HEAD before failing.
func TestRun_ImplementStage_AgentFailure_NoPR_SkipsRestore(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: errors.New("agent crash"),
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	restoredRefs := installMovingHeadSpy(t, "operator-main", "agent-branch")

	var stderr strings.Builder
	if got := runFailedImplementStage(t, &stderr, "--no-pr"); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure on agent crash:\n%s", got, stderr.String())
	}
	if len(*restoredRefs) != 0 {
		t.Errorf("restoredRefs = %v, want none — --no-pr opts out of restoration entirely", *restoredRefs)
	}
	if strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("working_tree_restored must NOT be emitted under --no-pr:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_AgentFailure_RecaptureFails_SkipsRestore pins the
// #953 blind-checkout guard: when the run()-level net's in-defer HEAD re-read
// fails, it must NOT fall back to a blind `git checkout --force` (destroying
// uncommitted work is worse than leaving the operator stranded) — it emits
// working_tree_restore_failed and skips.
func TestRun_ImplementStage_AgentFailure_RecaptureFails_SkipsRestore(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: errors.New("agent crash"),
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	// First capture (pre-agent) succeeds; the defer's re-read fails.
	captureCalls := 0
	restoreCalls := 0
	origCap, origRes := captureHead, restoreHead
	captureHead = func(_ context.Context, _ string) (string, bool, error) {
		captureCalls++
		if captureCalls == 1 {
			return "operator-main", false, nil
		}
		return "", false, errors.New("symbolic-ref failed in defer")
	}
	restoreHead = func(_ context.Context, _, _ string) error { restoreCalls++; return nil }
	t.Cleanup(func() { captureHead = origCap; restoreHead = origRes })

	var stderr strings.Builder
	if got := runFailedImplementStage(t, &stderr); got != exitFailure {
		t.Fatalf("run = %d, want exitFailure on agent crash:\n%s", got, stderr.String())
	}
	if restoreCalls != 0 {
		t.Errorf("restoreHead called %d times, want 0 — never force-checkout blind when current HEAD is unknown", restoreCalls)
	}
	if !strings.Contains(stderr.String(), `"event":"working_tree_restore_failed"`) {
		t.Errorf("missing working_tree_restore_failed event for the skipped restore:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), `"event":"working_tree_restored"`) {
		t.Errorf("working_tree_restored must NOT be emitted when the re-read failed:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_Fixup_NoChanges_ReportsFixupNoChanges is the #856 fix:
// a fix-up re-dispatch that produces NO changes must NOT bare-return (the
// push_fixup trace gate left the stage in `running`, so a bare return hangs it
// until the SLA watchdog reaps it). It must report {outcome:"fixup_no_changes",
// branch, base_sha} — no HeadSHA, no PR artifact body — so the backend drives
// the fix-up stage terminal and re-parks the review gate.
func TestRun_ImplementStage_Fixup_NoChanges_ReportsFixupNoChanges(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	existingBranch := "fishhawk/run-11111111/stage-22222222"
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:     "22222222-3333-4444-5555-666666666666",
		StageType:   "implement",
		Prompt:      "implement",
		PromptHash:  "h",
		Fixup:       true,
		FixupBranch: existingBranch,
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{result: &gitops.CommitAndPushResult{NoChanges: true, BaseSHA: "base"}}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR should not be called for a no-changes fix-up")
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (fixup_no_changes report) must be called — a no-changes fix-up must not bare-return (#856)")
	}
	if fu.gotPRArgs.Outcome != "fixup_no_changes" {
		t.Errorf("report outcome = %q, want %q", fu.gotPRArgs.Outcome, "fixup_no_changes")
	}
	if fu.gotPRArgs.Branch != existingBranch {
		t.Errorf("report branch = %q, want %q", fu.gotPRArgs.Branch, existingBranch)
	}
	if fu.gotPRArgs.HeadSHA != "" {
		t.Errorf("report head_sha = %q, want empty (no commit landed)", fu.gotPRArgs.HeadSHA)
	}
	if len(fu.gotPRArgs.Body) != 0 {
		t.Errorf("report must not carry a PR artifact body, got %d bytes", len(fu.gotPRArgs.Body))
	}
	if !strings.Contains(stderr.String(), `"event":"implement_fixup_no_changes"`) {
		t.Errorf("missing implement_fixup_no_changes log line:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"implement_fixup_no_changes_reported"`) {
		t.Errorf("missing implement_fixup_no_changes_reported log line:\n%s", stderr.String())
	}
}

// TestRun_ImplementStage_FixupPushFailure_ReportsFailed is the #794 analogue
// of the decomposed-child push-failure test: when a fix-up re-dispatch's
// commit/push fails (e.g. the #728 compile gate blocks a drift-incomplete tree
// → category B, or a generic git/network error → category C), the runner
// reports the failure to /pull-request with Outcome=="failed" so the backend
// fails the fix-up stage its push_fixup trace gate left in `running` and fires
// #788 fix-up recovery — instead of leaving the trace-time succeeded zombie
// whose implement re-review approves an unlanded diff.
func TestRun_ImplementStage_FixupPushFailure_ReportsFailed(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	withFakeRemoteBranchExists(t, true)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:     "22222222-3333-4444-5555-666666666666",
		StageType:   "implement",
		Prompt:      "implement",
		PromptHash:  "h",
		Fixup:       true,
		FixupBranch: "fishhawk/run-11111111/stage-22222222",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: errors.New("ssh-agent fetch failed")}
	withFakeGitOps(t, fp, &fakePROpener{})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after a fix-up push failure (#794)")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "C" {
		t.Errorf("fix-up push failure report = {outcome:%q, category:%q}, want {failed, C}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
}

// TestRun_ImplementStage_FixupCreatedOutOfScope_CategoryB is the #818
// downstream-classification seam: when a fix-up's pre-push gate returns
// gitops.ErrFixupCreatedOutOfScope (the fix-up created net-new out-of-scope
// files that StageScoped would silently strip), the implement stage must
// surface FAILED with category B and report the failure via the /pull-request
// path (outcome=failed / category=B, no PR artifact body) so the backend's
// #788 fix-up recovery restores the run to its pre-fix-up review gate. The
// fakePusher returns the sentinel exactly as the real gitops.Pusher does when
// its VerifyCommit hook fails before the push.
func TestRun_ImplementStage_FixupCreatedOutOfScope_CategoryB(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:     "22222222-3333-4444-5555-666666666666",
		StageType:   "implement",
		Prompt:      "implement",
		PromptHash:  "h",
		Fixup:       true,
		FixupBranch: "fishhawk/run-11111111/stage-22222222",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrFixupCreatedOutOfScope}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category-B on fix-up created-out-of-scope failure, got:\n%s", stderr.String())
	}
	// A fix-up never opens a fresh PR.
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not be called for a fix-up pass")
	}
	// #788 recovery hinges on the /pull-request FAILURE report (outcome=failed,
	// category=B, no artifact body) being driven by the fix-up push path.
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after a fix-up gate failure")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
	if len(fu.gotPRArgs.Body) != 0 {
		t.Errorf("failure report must not carry a PR artifact body, got %d bytes", len(fu.gotPRArgs.Body))
	}
}

// TestRun_ImplementStage_CreatedOutOfScope_CategoryB is the #825 open-PR
// analogue of TestRun_ImplementStage_FixupCreatedOutOfScope_CategoryB: when a
// NON-fix-up open-PR push's pre-push gate returns gitops.ErrCreatedOutOfScope
// (the stage created net-new out-of-scope files StageScoped would silently
// strip), the implement stage must surface FAILED with category B and report the
// failure via the /pull-request path (outcome=failed / category=B, no PR
// artifact body) so the gated stage transitions instead of hanging — and must
// NOT open a fresh PR for the failed push.
func TestRun_ImplementStage_CreatedOutOfScope_CategoryB(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrCreatedOutOfScope}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category-B on open-PR created-out-of-scope failure, got:\n%s", stderr.String())
	}
	// The push failed before any PR could be opened — no fresh PR for a failed push.
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not be called after a failed open-PR push")
	}
	// The gated open-PR stage transitions via the /pull-request FAILURE report
	// (outcome=failed, category=B, no artifact body).
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after an open-PR gate failure")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
	if len(fu.gotPRArgs.Body) != 0 {
		t.Errorf("failure report must not carry a PR artifact body, got %d bytes", len(fu.gotPRArgs.Body))
	}
}

// TestRun_ImplementStage_BaseRebaseConflict_CategoryB is the #866 analogue: when
// the open-PR push path PERSISTENTLY returns gitops.ErrBaseRebaseConflict (the
// fresh-fetch base path could not reapply the agent's stashed edits onto the
// diverged authoritative base), the implement stage gets exactly ONE bounded
// agent re-invoke on the fresh base (#989) and then surfaces FAILED with
// category B unchanged, reporting the failure via the /pull-request path
// (outcome=failed / category=B, no PR artifact body) — exercising both the
// gitops-sentinel -> FailureCategory classification seam and the re-invoke's
// boundedness.
func TestRun_ImplementStage_BaseRebaseConflict_CategoryB(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fi := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, fi)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrBaseRebaseConflict}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category-B on base-rebase-conflict failure, got:\n%s", stderr.String())
	}
	// Bounded re-invoke (#989): the persistent conflict yields exactly ONE
	// extra agent invocation (base + one re-invoke) and exactly TWO
	// CommitAndPush attempts before the unchanged failure path.
	if fi.callIdx != 2 {
		t.Errorf("invoker called %d times, want 2 (base invoke + one bounded re-invoke)", fi.callIdx)
	}
	if fp.calls != 2 {
		t.Errorf("CommitAndPush called %d times, want 2 (initial + one bounded retry)", fp.calls)
	}
	if !strings.Contains(stderr.String(), `"event":"base_rebase_conflict_reinvoke"`) {
		t.Errorf("expected base_rebase_conflict_reinvoke event in the log, got:\n%s", stderr.String())
	}
	// The push failed before any PR could be opened — no fresh PR for a failed push.
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not be called after a failed base-rebase push")
	}
	// The gated open-PR stage transitions via the /pull-request FAILURE report.
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after a base-rebase gate failure")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
	if len(fu.gotPRArgs.Body) != 0 {
		t.Errorf("failure report must not carry a PR artifact body, got %d bytes", len(fu.gotPRArgs.Body))
	}
}

// TestRun_ImplementStage_BaseRebaseConflict_ReinvokeSucceeds is the #989
// success regression for the issue's exact shape (run 8342436e / child
// 4e595927): the first CommitAndPush fails with a typed
// *BaseRebaseConflictError carrying realistic conflict context, the agent is
// re-invoked exactly once with a prompt embedding that context, and the
// second CommitAndPush succeeds — the stage exits OK and the push/PR/artifact
// chain completes. This crosses the gitops error-production and runner
// error-consumption layers: a typed error losing its Unwrap chain or its
// context fields fails here even when each layer's units pass.
func TestRun_ImplementStage_BaseRebaseConflict_ReinvokeSucceeds(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fi := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, fi)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	conflict := &gitops.BaseRebaseConflictError{
		ConflictPaths: []string{"registry.txt"},
		ConflictHunks: "++<<<<<<< Updated upstream\n +child-one addition\n++=======\n+ child-two addition\n++>>>>>>> Stashed changes",
		StashPatch:    "+child-two addition",
	}
	fp := &fakePusher{errSeq: []error{conflict, nil}}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fi.callIdx != 2 {
		t.Errorf("invoker called %d times, want 2 (base invoke + one re-invoke)", fi.callIdx)
	}
	if fp.calls != 2 {
		t.Errorf("CommitAndPush called %d times, want 2 (conflict + successful retry)", fp.calls)
	}
	// The re-invoke prompt embeds the captured conflict context: the
	// conflicted path and the agent's own stashed patch. gotInv records the
	// LAST invocation — the re-invoke.
	if fi.gotInv == nil {
		t.Fatal("no invocation captured")
	}
	if !strings.Contains(fi.gotInv.Prompt, "registry.txt") {
		t.Errorf("re-invoke prompt must name the conflicted path, got:\n%s", fi.gotInv.Prompt)
	}
	if !strings.Contains(fi.gotInv.Prompt, "child-two addition") {
		t.Errorf("re-invoke prompt must embed the stashed patch, got:\n%s", fi.gotInv.Prompt)
	}
	if !strings.Contains(stderr.String(), `"event":"base_rebase_conflict_reinvoke"`) {
		t.Errorf("expected base_rebase_conflict_reinvoke event in the log, got:\n%s", stderr.String())
	}
	// The retried push landed and the PR + artifact chain completed.
	if fpr.gotArgs == nil {
		t.Fatal("OpenPR must be called after the successful retried push")
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (artifact) must be called after the successful retried push")
	}
	if fu.gotPRArgs.Outcome != "" || len(fu.gotPRArgs.Body) == 0 {
		t.Errorf("artifact upload = {outcome:%q, body:%d bytes}, want a success-path artifact body",
			fu.gotPRArgs.Outcome, len(fu.gotPRArgs.Body))
	}
}

// TestRun_ImplementStage_Fixup_BaseRebaseConflict_NoReinvoke pins the #989
// fix-up exclusion: a fix-up pass hitting the same conflict error must NOT
// re-invoke the agent — it keeps the existing immediate category-B failure
// whose /pull-request report drives the backend's #788 fix-up recovery.
func TestRun_ImplementStage_Fixup_BaseRebaseConflict_NoReinvoke(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fi := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, fi)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:     "22222222-3333-4444-5555-666666666666",
		StageType:   "implement",
		Prompt:      "implement",
		PromptHash:  "h",
		Fixup:       true,
		FixupBranch: "fishhawk/run-11111111/stage-22222222",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrBaseRebaseConflict}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if fi.callIdx != 1 {
		t.Errorf("invoker called %d times, want 1 (no re-invoke on the fix-up path)", fi.callIdx)
	}
	if fp.calls != 1 {
		t.Errorf("CommitAndPush called %d times, want 1 (no retry on the fix-up path)", fp.calls)
	}
	if strings.Contains(stderr.String(), `"event":"base_rebase_conflict_reinvoke"`) {
		t.Errorf("fix-up path must not emit base_rebase_conflict_reinvoke, got:\n%s", stderr.String())
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after a fix-up base-rebase failure")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
}

// TestRun_ImplementStage_BaseRebaseConflict_ReinvokeNotOK pins the #989
// non-OK re-invoke result path: the re-invoked agent's invocation succeeds at
// the transport level but the agent reports OK=false (declined / failed
// semantically). The runner must NOT retry the push with a tree the agent did
// not vouch for — it aborts the re-invoke (base_rebase_reinvoke_aborted) and
// falls through to the unchanged category-B failure with the ORIGINAL
// conflict error, after exactly ONE CommitAndPush attempt.
func TestRun_ImplementStage_BaseRebaseConflict_ReinvokeNotOK(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fi := &fakeInvoker{cannedSeq: []agent.Result{
		{OK: true},
		{OK: false, FailureCategory: "A", FailureReason: "agent declined the re-land"},
	}}
	withFakeInvoker(t, fi)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrBaseRebaseConflict}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if fi.callIdx != 2 {
		t.Errorf("invoker called %d times, want 2 (base invoke + one re-invoke)", fi.callIdx)
	}
	// The non-OK re-invoke result must NOT earn a second push attempt.
	if fp.calls != 1 {
		t.Errorf("CommitAndPush called %d times, want 1 (no retry after a non-OK re-invoke)", fp.calls)
	}
	if !strings.Contains(stderr.String(), `"event":"base_rebase_reinvoke_aborted"`) {
		t.Errorf("expected base_rebase_reinvoke_aborted event in the log, got:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "agent declined the re-land") {
		t.Errorf("abort detail must carry the agent's failure reason, got:\n%s", stderr.String())
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not be called after a failed base-rebase push")
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after the aborted re-invoke")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
}

// TestRun_ImplementStage_BaseRebaseConflict_CheckoutFails pins the #989
// checkout-failure degradation: when re-checking-out the run branch fails,
// the re-invoke aborts BEFORE any agent invocation and the stage degrades to
// the unchanged category-B failure with the original conflict error.
func TestRun_ImplementStage_BaseRebaseConflict_CheckoutFails(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fi := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, fi)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrBaseRebaseConflict}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)
	// Swap in the failing checkout AFTER withFakeGitOps installs its no-op.
	checkoutRunBranch = func(_ context.Context, _, _ string) error {
		return errors.New("ref lock contention")
	}

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if fi.callIdx != 1 {
		t.Errorf("invoker called %d times, want 1 (checkout failed before any re-invoke)", fi.callIdx)
	}
	if fp.calls != 1 {
		t.Errorf("CommitAndPush called %d times, want 1 (no retry after a checkout failure)", fp.calls)
	}
	if !strings.Contains(stderr.String(), `"event":"base_rebase_reinvoke_aborted"`) {
		t.Errorf("expected base_rebase_reinvoke_aborted event in the log, got:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "checkout run branch") {
		t.Errorf("abort detail must name the checkout failure, got:\n%s", stderr.String())
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after the aborted re-invoke")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
}

// TestRun_ImplementStage_BaseRebaseConflict_ReinvokeInfraExhausted pins the
// #989 infra-exhaustion degradation: every re-invocation attempt fails at the
// transport level. Each failed attempt emits base_rebase_reinvoke_error
// (maxFixInvokeInfraRetries total, the #804 pattern), then the re-invoke
// aborts (base_rebase_reinvoke_aborted) and the stage degrades to the
// unchanged category-B failure with the original conflict error.
func TestRun_ImplementStage_BaseRebaseConflict_ReinvokeInfraExhausted(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	// Call 1 (base invoke) succeeds; the trailing entry repeats, so every
	// re-invocation attempt errors at the transport level.
	fi := &fakeInvoker{
		cannedSeq: []agent.Result{{OK: true}, {}},
		errSeq:    []error{nil, errors.New("agent API 400: thinking block")},
	}
	withFakeInvoker(t, fi)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrBaseRebaseConflict}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if want := 1 + maxFixInvokeInfraRetries; fi.callIdx != want {
		t.Errorf("invoker called %d times, want %d (base invoke + %d infra-retried re-invoke attempts)",
			fi.callIdx, want, maxFixInvokeInfraRetries)
	}
	if fp.calls != 1 {
		t.Errorf("CommitAndPush called %d times, want 1 (no retry after infra exhaustion)", fp.calls)
	}
	if got := strings.Count(stderr.String(), `"event":"base_rebase_reinvoke_error"`); got != maxFixInvokeInfraRetries {
		t.Errorf("base_rebase_reinvoke_error emitted %d times, want %d (one per failed attempt), got:\n%s",
			got, maxFixInvokeInfraRetries, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"base_rebase_reinvoke_aborted"`) {
		t.Errorf("expected base_rebase_reinvoke_aborted event in the log, got:\n%s", stderr.String())
	}
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after infra exhaustion")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
}

// captureImplementVerifyCommit runs the implement upload flow with a fake
// pusher pointed at repo and returns the verifyCommit closure the runner wires
// into CommitAndPush, so the created-out-of-scope gate logic (#818, generalized
// to the open-PR path by #825) can be exercised directly against a real working
// tree. fixup toggles the fix-up prompt path; decomposed routes the run as a
// decomposed child (the gate is excluded on that path). fixup and decomposed
// are mutually exclusive.
func captureImplementVerifyCommit(t *testing.T, repo string, fixup, decomposed bool) func(context.Context, string, []string) error {
	t.Helper()
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	pr := &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	if fixup {
		pr.Fixup = true
		pr.FixupBranch = "fishhawk/run-11111111/stage-22222222"
	}
	if decomposed {
		pr.DecomposedFromRunID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		withFakeRemoteBranchExists(t, false) // first child: deterministic routing
	}
	fu.promptResp = pr
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	withFakeGitOps(t, fp, &fakePROpener{})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--working-dir", repo,
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil || fp.gotArgs.VerifyCommit == nil {
		t.Fatal("VerifyCommit hook not captured")
	}
	return fp.gotArgs.VerifyCommit
}

// TestRun_ImplementStage_FixupCreatedOutOfScopeGate exercises the
// created-out-of-scope gate decision (error vs nil) directly through the wired
// verifyCommit closure against a real working tree, on both the fix-up path
// (#818) and the open-PR path (#825). The "fails BEFORE push (origin
// untouched)" property is the contract of CommitAndPush — a VerifyCommit error
// aborts before the push — and is proven in gitops by
// TestCommitAndPush_VerifyCommit_AbortsBeforePush; here we prove the closure
// returns that error for exactly the right working-tree shapes.
func TestRun_ImplementStage_FixupCreatedOutOfScopeGate(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// (a) A fix-up whose working tree carries an untracked out-of-scope created
	// file fails with ErrFixupCreatedOutOfScope, naming the file.
	t.Run("created_out_of_scope_fails", func(t *testing.T) {
		repo, runGit := compileGateRepo(t)
		mustWrite(t, filepath.Join(repo, "README.md"), "# init\n")
		runGit("add", "-A")
		runGit("commit", "-m", "init")
		mustWrite(t, filepath.Join(repo, "newfile.go"), "package x\n") // untracked, out of scope
		head := gitHead(t, repo)

		vc := captureImplementVerifyCommit(t, repo, true, false)
		err := vc(context.Background(), head, []string{"newfile.go"})
		if !errors.Is(err, gitops.ErrFixupCreatedOutOfScope) {
			t.Fatalf("err = %v, want ErrFixupCreatedOutOfScope", err)
		}
		if !strings.Contains(err.Error(), "newfile.go") {
			t.Errorf("error should name the created file newfile.go: %v", err)
		}
	})

	// (b) A fix-up that creates only in-scope files leaves no out-of-scope
	// drift (StageScoped staged them), so the gate does not fire.
	t.Run("in_scope_only_passes", func(t *testing.T) {
		repo, runGit := compileGateRepo(t)
		mustWrite(t, filepath.Join(repo, "README.md"), "# init\n")
		runGit("add", "-A")
		runGit("commit", "-m", "init")
		head := gitHead(t, repo)

		vc := captureImplementVerifyCommit(t, repo, true, false)
		if err := vc(context.Background(), head, nil); err != nil {
			t.Errorf("empty drift must pass the gate, got: %v", err)
		}
	})

	// (c) A fix-up with only modified-but-out-of-scope drift (a tracked file)
	// still passes — modified-out-of-scope stays flag-only (ADR-027); only
	// CREATED (untracked) files trip the gate.
	t.Run("modified_out_of_scope_stays_flag_only", func(t *testing.T) {
		repo, runGit := compileGateRepo(t)
		mustWrite(t, filepath.Join(repo, "README.md"), "# init\n")
		runGit("add", "-A")
		runGit("commit", "-m", "init")
		mustWrite(t, filepath.Join(repo, "README.md"), "# modified\n") // tracked, modified
		head := gitHead(t, repo)

		vc := captureImplementVerifyCommit(t, repo, true, false)
		if err := vc(context.Background(), head, []string{"README.md"}); err != nil {
			t.Errorf("modified-out-of-scope drift must stay flag-only, got: %v", err)
		}
	})

	// (d) The SAME untracked out-of-scope working tree on a NON-fix-up OPEN-PR
	// implement push NOW fails the gate (#825) with the general
	// ErrCreatedOutOfScope sentinel — matching ErrCreatedOutOfScope but NOT the
	// fix-up specialization ErrFixupCreatedOutOfScope — naming the file.
	t.Run("open_pr_created_out_of_scope_fails", func(t *testing.T) {
		repo, runGit := compileGateRepo(t)
		mustWrite(t, filepath.Join(repo, "README.md"), "# init\n")
		runGit("add", "-A")
		runGit("commit", "-m", "init")
		mustWrite(t, filepath.Join(repo, "newfile.go"), "package x\n") // untracked, out of scope
		head := gitHead(t, repo)

		vc := captureImplementVerifyCommit(t, repo, false, false)
		err := vc(context.Background(), head, []string{"newfile.go"})
		if !errors.Is(err, gitops.ErrCreatedOutOfScope) {
			t.Fatalf("err = %v, want ErrCreatedOutOfScope", err)
		}
		if errors.Is(err, gitops.ErrFixupCreatedOutOfScope) {
			t.Errorf("open-PR push must wrap the general sentinel, not the fix-up specialization: %v", err)
		}
		if !strings.Contains(err.Error(), "newfile.go") {
			t.Errorf("error should name the created file newfile.go: %v", err)
		}
	})

	// (e) An open-PR push with only modified-but-out-of-scope drift (a tracked
	// file) passes — modified-out-of-scope stays flag-only (ADR-027) on the
	// open-PR path too; only CREATED files trip the gate.
	t.Run("open_pr_modified_out_of_scope_stays_flag_only", func(t *testing.T) {
		repo, runGit := compileGateRepo(t)
		mustWrite(t, filepath.Join(repo, "README.md"), "# init\n")
		runGit("add", "-A")
		runGit("commit", "-m", "init")
		mustWrite(t, filepath.Join(repo, "README.md"), "# modified\n") // tracked, modified
		head := gitHead(t, repo)

		vc := captureImplementVerifyCommit(t, repo, false, false)
		if err := vc(context.Background(), head, []string{"README.md"}); err != nil {
			t.Errorf("modified-out-of-scope drift must stay flag-only on the open-PR path, got: %v", err)
		}
	})

	// (f) An open-PR push with empty/in-scope drift passes the gate.
	t.Run("open_pr_in_scope_only_passes", func(t *testing.T) {
		repo, runGit := compileGateRepo(t)
		mustWrite(t, filepath.Join(repo, "README.md"), "# init\n")
		runGit("add", "-A")
		runGit("commit", "-m", "init")
		head := gitHead(t, repo)

		vc := captureImplementVerifyCommit(t, repo, false, false)
		if err := vc(context.Background(), head, nil); err != nil {
			t.Errorf("empty drift must pass the open-PR gate, got: %v", err)
		}
	})

	// (g) A DECOMPOSED-CHILD push with the SAME untracked out-of-scope created
	// file does NOT trip the gate (#825 ASSUMPTION #3): a child may legitimately
	// create files a later child declares, so the shared-branch path tolerates
	// net-new out-of-scope files. This pins the `!isDecomposed` term — a future
	// edit dropping it is caught here.
	t.Run("decomposed_child_created_out_of_scope_passes", func(t *testing.T) {
		repo, runGit := compileGateRepo(t)
		mustWrite(t, filepath.Join(repo, "README.md"), "# init\n")
		runGit("add", "-A")
		runGit("commit", "-m", "init")
		mustWrite(t, filepath.Join(repo, "newfile.go"), "package x\n") // untracked, out of scope
		head := gitHead(t, repo)

		vc := captureImplementVerifyCommit(t, repo, false, true)
		if err := vc(context.Background(), head, []string{"newfile.go"}); err != nil {
			t.Errorf("decomposed-child push must not trip the created-out-of-scope gate, got: %v", err)
		}
	})
}

// --- Verify gate (#441) ---

// TestVerify_NoCmd_SkipsGate confirms that when --verify-cmd is absent
// run() returns exitOK and the bundle contains no verify_run event.
func TestVerify_NoCmd_SkipsGate(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)

	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--bundle-out", bundlePath,
		// --verify-cmd intentionally absent
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	_, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == "verify_run" {
			t.Errorf("unexpected verify_run event in bundle when --verify-cmd is absent: %+v", ev)
		}
	}
}

// TestVerify_Passes confirms that --verify-cmd 'true' exits 0, the
// run succeeds, and the bundle contains a verify_run event with outcome=passed.
func TestVerify_Passes(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)

	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--bundle-out", bundlePath,
		"--verify-cmd", "true",
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	_, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatal(err)
	}
	var sawPassed bool
	for _, ev := range events {
		if ev.Kind == "verify_run" && strings.Contains(string(ev.Data), `"outcome":"passed"`) {
			sawPassed = true
		}
	}
	if !sawPassed {
		t.Errorf("missing verify_run outcome=passed in bundle:\n%+v", events)
	}
}

// TestVerify_Fails confirms that --verify-cmd 'false' exits 1, the run
// exits with exitFailure, the bundle manifest marks agent_failed=true,
// and the bundle contains a verify_run event with outcome=failed.
func TestVerify_Fails(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")
	_ = os.WriteFile(promptPath, []byte("p"), 0o600)

	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", promptPath,
		"--bundle-out", bundlePath,
		"--verify-cmd", "false",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("expected category-A on verify failure, got:\n%s", stderr.String())
	}

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.AgentFailed {
		t.Errorf("manifest.AgentFailed = false, want true on verify failure")
	}
	var sawFailed bool
	for _, ev := range events {
		if ev.Kind == "verify_run" && strings.Contains(string(ev.Data), `"outcome":"failed"`) {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Errorf("missing verify_run outcome=failed in bundle:\n%+v", events)
	}
}

// TestVerify_CtxCancelledMidGate confirms that when the runner context
// is cancelled while the verify command is running, run() exits with
// exitCancelled (130) and emits a runner_cancelled log line.
func TestVerify_CtxCancelledMidGate(t *testing.T) {
	prompt := filepath.Join(t.TempDir(), "prompt.txt")
	_ = os.WriteFile(prompt, []byte("p"), 0o600)

	// Agent returns immediately so the verify gate is reached.
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	cancel := withCancelableRunnerContext(t)

	// Cancel after a brief delay so the verify command is running.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var out strings.Builder
	got := run([]string{
		"--run-id", "rid", "--backend-url", "u",
		"--workflow", "w", "--stage", "s",
		"--prompt-file", prompt,
		"--verify-cmd", "sleep 60",
	}, &out)

	if got != exitCancelled {
		t.Errorf("run = %d, want exitCancelled(%d)", got, exitCancelled)
	}
	if !strings.Contains(out.String(), `"event":"runner_cancelled"`) {
		t.Errorf("missing runner_cancelled log line:\n%s", out.String())
	}
}

// TestRun_FetchPrompt_OperatorTimeoutWins verifies that when --timeout is
// passed explicitly, the operator value wins over the server-resolved
// AgentTimeoutSeconds, regardless of what the server returns.
func TestRun_FetchPrompt_OperatorTimeoutWins(t *testing.T) {
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             "22222222-3333-4444-5555-666666666666",
		StageType:           "plan",
		Prompt:              "Hello agent.",
		PromptHash:          "deadbeef",
		AgentTimeoutSeconds: 1800, // server says 30m
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--timeout", "5m", // operator explicitly overrides
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if invoker.gotInv == nil {
		t.Fatal("invoker.gotInv nil — invocation not captured")
	}
	if invoker.gotInv.Budget.Timeout != 5*time.Minute {
		t.Errorf("Budget.Timeout = %v, want 5m (operator flag wins over server timeout)",
			invoker.gotInv.Budget.Timeout)
	}
}

// --- Verify config wire (#504) ---

// TestRun_FetchPrompt_SpecVerifyCmd_NoFlag confirms that when the prompt
// response carries VerifyCommand and no --verify-cmd flag is set, the verify
// gate fires with the spec-sourced command.
func TestRun_FetchPrompt_SpecVerifyCmd_NoFlag(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")

	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:              "22222222-3333-4444-5555-666666666666",
		StageType:            "plan",
		Prompt:               "Hello agent.",
		PromptHash:           "deadbeef",
		VerifyCommand:        "true", // exits 0 — verify passes
		VerifyTimeoutSeconds: 30,
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--bundle-out", bundlePath,
		// --verify-cmd intentionally absent; spec-sourced command applies
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (spec verify cmd 'true'):\n%s", got, stderr.String())
	}

	// The bundle must contain a verify_run event proving the gate fired.
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("bundle not written: %v", err)
	}
	_, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	var sawVerify bool
	for _, ev := range events {
		if ev.Kind == "verify_run" && strings.Contains(string(ev.Data), `"outcome":"passed"`) {
			sawVerify = true
		}
	}
	if !sawVerify {
		t.Errorf("missing verify_run outcome=passed in bundle (spec verify cmd should have fired):\n%+v", events)
	}
}

// TestRun_FetchPrompt_OperatorVerifyCmdWins confirms that when --verify-cmd
// is set explicitly, the operator flag wins over the spec-sourced command.
func TestRun_FetchPrompt_OperatorVerifyCmdWins(t *testing.T) {
	dir := t.TempDir()
	bundlePath := filepath.Join(dir, "trace.jsonl.gz")

	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:       "22222222-3333-4444-5555-666666666666",
		StageType:     "plan",
		Prompt:        "Hello agent.",
		PromptHash:    "deadbeef",
		VerifyCommand: "false", // spec says 'false' — would fail if used
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--bundle-out", bundlePath,
		"--verify-cmd", "true", // operator override wins
	}, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (operator 'true' should win over spec 'false'):\n%s", got, stderr.String())
	}

	// Verify that the bundle records the operator-supplied command, not the spec command.
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("bundle not written: %v", err)
	}
	_, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	var sawPassed bool
	for _, ev := range events {
		if ev.Kind == "verify_run" {
			payload := string(ev.Data)
			if strings.Contains(payload, `"command":"true"`) && strings.Contains(payload, `"outcome":"passed"`) {
				sawPassed = true
			}
		}
	}
	if !sawPassed {
		t.Errorf("expected verify_run with command=true outcome=passed (operator wins):\n%+v", events)
	}
}

// --- Committed-tree verify-fix loop (#651) ---

// verifyFixBaseRepo inits a real git repo with a committed go.work + module
// skeleton (go.work, mod/go.mod) — the "base branch" the implement stage
// builds on. The scope files (mod/reg.go, mod/reg_test.go) and any drift are
// left for the caller to write into the working tree, simulating the agent's
// uncommitted edits. Skips when go or git is unavailable.
func verifyFixBaseRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	runGit("add", "-A")
	runGit("commit", "-m", "base: go.work + empty module")
	return repo
}

// regGetBuggy: the registry is empty, so TestGet (wants 42) fails on the
// committed scope-only tree. regGetFixed adds the seeding init INSIDE the
// scope file so the committed tree passes without the dropped drift helper.
const regGetBuggy = "package mod\n\nvar registry = map[string]int{}\n\nfunc Get(k string) int { return registry[k] }\n"
const regGetFixed = "package mod\n\nvar registry = map[string]int{}\n\nfunc init() { registry[\"x\"] = 42 }\n\nfunc Get(k string) int { return registry[k] }\n"
const regGetTest = "package mod\n\nimport \"testing\"\n\nfunc TestGet(t *testing.T) {\n\tif Get(\"x\") != 42 {\n\t\tt.Fatalf(\"Get(x) = %d, want 42\", Get(\"x\"))\n\t}\n}\n"

// assertVerifySummary finds the verify_summary trace event and checks its
// outcome + iteration count (#651 audit-trace requirement).
func assertVerifySummary(t *testing.T, events []bundle.Line, wantOutcome string, wantIterations, wantMax int) {
	t.Helper()
	for _, ev := range events {
		if ev.Kind != "verify_summary" {
			continue
		}
		var got struct {
			Outcome       string `json:"outcome"`
			Iterations    int    `json:"iterations"`
			MaxIterations int    `json:"max_iterations"`
		}
		if err := json.Unmarshal(ev.Data, &got); err != nil {
			t.Fatalf("verify_summary payload unmarshal: %v (%s)", err, ev.Data)
		}
		if got.Outcome != wantOutcome || got.Iterations != wantIterations || got.MaxIterations != wantMax {
			t.Errorf("verify_summary = %+v, want {outcome:%q iterations:%d max_iterations:%d}",
				got, wantOutcome, wantIterations, wantMax)
		}
		return
	}
	t.Errorf("no verify_summary event in bundle:\n%+v", events)
}

func readBundleEvents(t *testing.T, path string) []bundle.Line {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	_, events, _, err := openBundleForTest(data)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	return events
}

const verifyFixRunID = "11111111-2222-3333-4444-555555555555"
const verifyFixStageID = "22222222-3333-4444-5555-666666666666"

func verifyFixRunArgs(repo, bundlePath string) []string {
	return []string{
		"--run-id", verifyFixRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", verifyFixStageID,
		"--working-dir", repo,
		"--fetch-prompt", "--upload-trace",
		"--bundle-out", bundlePath,
	}
}

// TestRun_VerifyFixLoop_FailThenPass_CommittedTree is the #651 cross-boundary
// e2e. It seeds a DRIFT-EXCLUDED failure: a test that fails on the committed
// scope-only tree (the seeding helper seed.go is out of scope, so StageScoped
// drops it) but PASSES on the agent's full working tree. The loop must run
// against the committed tree (would false-green against the working tree),
// re-invoke the agent with the captured output, converge within the budget,
// and proceed to the push.
func TestRun_VerifyFixLoop_FailThenPass_CommittedTree(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	regPath := filepath.Join(repo, "mod", "reg.go")
	// Working tree (agent's uncommitted edits): scope files + a DRIFT helper
	// (seed.go) that makes the WORKING tree green but is dropped from the
	// committed scope-only tree.
	mustWrite(t, regPath, regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
	mustWrite(t, filepath.Join(repo, "mod", "seed.go"),
		"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")

	invoker := &fakeInvoker{
		canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, _ agent.Invocation) {
			// idx 0 is the initial agent (files seeded above). idx 1 is the fix
			// re-invoke: bring the fix into a SCOPE file so the committed
			// scope-only tree passes without the dropped drift helper.
			if idx == 1 {
				mustWrite(t, regPath, regGetFixed)
			}
		},
	}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 2,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	// Proves committed-tree execution: a working-tree verify would pass on
	// iteration 0 (seed.go present) and never re-invoke. Two invocations
	// (initial + one fix) means iteration 0 failed against the committed tree.
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + one fix)", invoker.callIdx)
	}
	// Push proceeded after convergence.
	if fp.gotArgs == nil {
		t.Error("CommitAndPush should run after the verify-fix loop converges")
	}
	if fpr.gotArgs == nil {
		t.Error("OpenPR should run after the verify-fix loop converges")
	}

	events := readBundleEvents(t, bundlePath)
	var sawFailed, sawPassed, sawHeadSHA bool
	for _, ev := range events {
		if ev.Kind != "verify_run" {
			continue
		}
		p := string(ev.Data)
		if strings.Contains(p, `"head_sha"`) {
			sawHeadSHA = true
		}
		if strings.Contains(p, `"outcome":"failed"`) {
			sawFailed = true
		}
		if strings.Contains(p, `"outcome":"passed"`) {
			sawPassed = true
		}
	}
	if !sawHeadSHA {
		t.Error("committed-tree verify_run events must carry head_sha")
	}
	if !sawFailed {
		t.Error("iteration 0 must fail against the committed tree (missing verify_run outcome=failed)")
	}
	if !sawPassed {
		t.Error("iteration 1 must pass after the fix (missing verify_run outcome=passed)")
	}
	assertVerifySummary(t, events, "passed", 2, 2)
}

// gitDiffPatches returns the `patch` text of every git_diff event in the
// bundle, in emission order. Used to assert the runner re-emits a reconciled
// scope-only git_diff after a verify-fix reinvoke (#870).
func gitDiffPatches(t *testing.T, events []bundle.Line) []string {
	t.Helper()
	var patches []string
	for _, ev := range events {
		if ev.Kind != "git_diff" {
			continue
		}
		var p struct {
			Patch string `json:"patch"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			t.Fatalf("git_diff payload unmarshal: %v (%s)", err, ev.Data)
		}
		patches = append(patches, p.Patch)
	}
	return patches
}

// TestRun_VerifyFixLoop_ReemitsReconciledGitDiff is the #870 cross-boundary
// proof: a verify-fix loop that reinvokes the agent rewrites an in-scope file,
// so the git_diff computeAndEmitDiff emitted BEFORE the loop is stale. The
// runner must re-emit a fresh scope-only git_diff AFTER the loop, and that
// last event must reflect the reconciled committed tree (the fix), so the
// backend's last-write-wins ExtractDiff feeds the implement review and policy
// re-eval the diff the PR actually ships. Mirrors the FailThenPass setup but
// passes --check-base-ref so the git_diff events are emitted.
func TestRun_VerifyFixLoop_ReemitsReconciledGitDiff(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	regPath := filepath.Join(repo, "mod", "reg.go")
	mustWrite(t, regPath, regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
	mustWrite(t, filepath.Join(repo, "mod", "seed.go"),
		"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")

	invoker := &fakeInvoker{
		canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, _ agent.Invocation) {
			if idx == 1 {
				mustWrite(t, regPath, regGetFixed)
			}
		},
	}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 2,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	args := append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}

	patches := gitDiffPatches(t, readBundleEvents(t, bundlePath))
	if len(patches) != 2 {
		t.Fatalf("git_diff event count = %d, want 2 (original + reconciled re-emit)", len(patches))
	}
	// The pre-reconcile diff carries the buggy reg.go (no seeding init); the
	// reconciled re-emit carries the fix. Last-write-wins makes the latter
	// authoritative for the implement review and policy re-eval.
	const fixMarker = `registry["x"] = 42`
	if strings.Contains(patches[0], fixMarker) {
		t.Errorf("first git_diff must be the PRE-reconcile (buggy) diff, but it carries the fix:\n%s", patches[0])
	}
	if !strings.Contains(patches[len(patches)-1], fixMarker) {
		t.Errorf("last git_diff must be the reconciled diff carrying the fix %q:\n%s", fixMarker, patches[len(patches)-1])
	}
}

// TestRun_VerifyFixLoop_PassFirstIteration_SingleGitDiff: a loop whose first
// committed-tree verify PASSES never reinvokes, so the runner must NOT re-emit
// a second git_diff — exactly one (computeAndEmitDiff's original) is in the
// bundle. Guards the strict reinvoked gate (#870).
func TestRun_VerifyFixLoop_PassFirstIteration_SingleGitDiff(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	// reg.go is correct on the committed scope-only tree from the start, so
	// iteration 0 passes and the loop never reinvokes.
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 2,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	args := append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if n := len(gitDiffPatches(t, readBundleEvents(t, bundlePath))); n != 1 {
		t.Fatalf("git_diff event count = %d, want 1 (no reinvoke → no re-emit)", n)
	}
	if invoker.callIdx != 1 {
		t.Errorf("Invoke call count = %d, want 1 (no fix reinvoke)", invoker.callIdx)
	}
}

// TestRun_VerifyGateCommitted_SingleShot_SingleGitDiff: the maxIterations==0
// single-shot committed gate (#802) has no fix loop, so it never reinvokes and
// must leave exactly one git_diff (computeAndEmitDiff's original) in the bundle.
func TestRun_VerifyGateCommitted_SingleShot_SingleGitDiff(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 0,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	args := append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if n := len(gitDiffPatches(t, readBundleEvents(t, bundlePath))); n != 1 {
		t.Fatalf("git_diff event count = %d, want 1 (single-shot gate, no re-emit)", n)
	}
}

// TestRun_VerifyFixLoop_Exhaustion_TerminalNoRetry is the both-enabled
// non-compounding proof (#651, DECISION c2): with agent_self_retry on AND a
// verify-fix budget, an agent that never fixes the failure must demote the
// stage TERMINALLY — capped at max_iterations+1 total invocations, NO
// RetryStage call, and no push.
func TestRun_VerifyFixLoop_Exhaustion_TerminalNoRetry(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	// A failing test the agent never fixes — the committed scope-only tree is
	// red on every iteration.
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{
		canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		// No onInvoke fix: the failure persists across all iterations.
	}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 2,
		// Both enabled: self-retry on AND a verify-fix budget. The verify-fix
		// loop lives outside the self-retry loop, so exhaustion must NOT call
		// RetryStage.
		AgentSelfRetry:     true,
		MaxRetriesSnapshot: 3,
		RetryAttempt:       0,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("expected category-A terminal demotion:\n%s", stderr.String())
	}
	// Non-compounding (DECISION c2): RetryStage NEVER called.
	if len(fu.gotRetryArgs) != 0 {
		t.Errorf("RetryStage called %d times; verify-fix exhaustion must be terminal (no RetryStage)", len(fu.gotRetryArgs))
	}
	// Total agent invocations capped at max_iterations+1 (initial + 2 fixes).
	if invoker.callIdx != 3 {
		t.Errorf("Invoke call count = %d, want 3 (initial + 2 fixes = max_iterations+1)", invoker.callIdx)
	}
	// Terminal: no push.
	if fp.gotArgs != nil {
		t.Error("CommitAndPush must not run after a terminal verify-fix exhaustion")
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not run after a terminal verify-fix exhaustion")
	}
	assertVerifySummary(t, readBundleEvents(t, bundlePath), "failed", 3, 2)
}

// TestRun_VerifyFixLoop_TransientFixInvokeError_RetriesWithoutBudgetBurn is the
// #804 Gap-1 transient case: the FIRST fix re-invocation returns an infra error
// (a #798-style agent-API blip); the in-place retry succeeds and the loop
// converges. The transient error must NOT consume a fix-loop budget unit — the
// verify-attempt count stays equal to the no-error FailThenPass case (2) — and
// it must be RECORDED as a verify_fix_reinvoke_error trace event + log line,
// never swallowed.
func TestRun_VerifyFixLoop_TransientFixInvokeError_RetriesWithoutBudgetBurn(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	regPath := filepath.Join(repo, "mod", "reg.go")
	mustWrite(t, regPath, regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
	mustWrite(t, filepath.Join(repo, "mod", "seed.go"),
		"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")

	invoker := &fakeInvoker{
		// idx 0: initial agent. idx 1: fix re-invoke #1 -> infra error (retried in
		// place). idx 2: fix re-invoke retry -> success.
		cannedSeq: []agent.Result{
			{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
			{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
			{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		},
		errSeq: []error{nil, agent.ErrAgentFailed, nil},
		onInvoke: func(idx int, _ agent.Invocation) {
			// Only the SUCCESSFUL fix invoke (idx 2) brings the fix into a scope
			// file; the errored attempt (idx 1) leaves the tree unchanged.
			if idx == 2 {
				mustWrite(t, regPath, regGetFixed)
			}
		},
	}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 2,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	// 3 invocations: initial + errored fix + retried fix. The no-error
	// FailThenPass case is 2 (initial + one fix); the extra call is the in-place
	// infra retry, NOT an extra verify iteration.
	if invoker.callIdx != 3 {
		t.Errorf("Invoke call count = %d, want 3 (initial + errored fix + retry)", invoker.callIdx)
	}
	if fp.gotArgs == nil {
		t.Error("CommitAndPush should run after convergence")
	}

	events := readBundleEvents(t, bundlePath)
	reinvokeErrs := 0
	for _, ev := range events {
		if ev.Kind == "verify_fix_reinvoke_error" {
			reinvokeErrs++
		}
	}
	if reinvokeErrs != 1 {
		t.Errorf("verify_fix_reinvoke_error events = %d, want 1 (recorded, not swallowed)", reinvokeErrs)
	}
	if !strings.Contains(stderr.String(), "verify_fix_reinvoke_error") {
		t.Errorf("missing verify_fix_reinvoke_error log line:\n%s", stderr.String())
	}
	// Budget NOT burned: still exactly 2 verify attempts (== the no-error case),
	// converged outcome.
	assertVerifySummary(t, events, "passed", 2, 2)
}

// The two VERBATIM #972 verify-gate failure outputs (operator-binding plan
// condition): the matcher must hold against the real observed text, not an
// abbreviation, so a testcontainers-go wording change fails this test instead
// of silently dropping coverage.
//
// flakeOutputApproval is the approval-package failure observed in the
// dogfood verify gate on 2026-06-10 ~10:11 (#972 bullet 1; captured from the
// gate's verify_run trace event, run 73456dc8).
const flakeOutputApproval = `ok  	github.com/kuhlman-labs/fishhawk/backend/internal/anthropic	3.988s
ok  	github.com/kuhlman-labs/fishhawk/backend/internal/apitoken	19.693s
?   	github.com/kuhlman-labs/fishhawk/backend/internal/apitoken/db	[no test files]
--- FAIL: TestPostgres_Submit_Idempotent (63.27s)
    postgres_test.go:135: start postgres: run postgres: generic container: start container: started hook: wait until ready: mapped port: check target: retries: 9, port: "invalid port", last err: get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/dd45dc0863d386b8e4a5e6a6a0829b4be99e4b5da54e667a192f6a142dfe5baf/json": context deadline exceeded
FAIL
FAIL	github.com/kuhlman-labs/fishhawk/backend/internal/approval	75.415s
?   	github.com/kuhlman-labs/fishhawk/backend/internal/approval/db	[no test files]
ok  	github.com/kuhlman-labs/fishhawk/backend/internal/artifact	12.374s`

// flakeOutputAudit is the audit-package failure observed in run 1cef465f's
// implement verify gate on 2026-06-10 ~12:06 (#972 bullet 2; captured from
// the failed iteration's verify_run trace event / stage_retried prior_reason).
const flakeOutputAudit = `ok  	github.com/kuhlman-labs/fishhawk/backend/internal/artifact	12.811s
?   	github.com/kuhlman-labs/fishhawk/backend/internal/artifact/db	[no test files]
--- FAIL: TestPostgres_ListAll_MixesBothChainsTimeDesc (63.19s)
    postgres_test.go:740: start postgres: run postgres: generic container: start container: started hook: wait until ready: mapped port: check target: retries: 9, port: "invalid port", last err: get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/a2d7f90285ac6d9be2e47ff50c4071c39f30cb2ba15f8600e2feaac1cbbf1625/json": context deadline exceeded
FAIL
FAIL	github.com/kuhlman-labs/fishhawk/backend/internal/audit	111.660s
?   	github.com/kuhlman-labs/fishhawk/backend/internal/audit/db	[no test files]`

func TestIsTestcontainersStartFlake(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   bool
	}{
		{"verbatim #972 approval-package failure", flakeOutputApproval, true},
		{"verbatim #972 audit-package failure", flakeOutputAudit, true},
		{"ordinary assertion failure", "--- FAIL: TestGet (0.00s)\n    reg_test.go:7: Get(x) = 0, want 42\nFAIL\nFAIL\texample.com/mod\t0.012s\nFAIL", false},
		{"deadline without container marker", "--- FAIL: TestFetch (30.00s)\n    fetch_test.go:42: Get \"http://example.com/api\": context deadline exceeded\nFAIL", false},
		{"container marker without deadline", "--- FAIL: TestStart (5.00s)\n    main_test.go:10: failed to start container: image not found\nFAIL", false},
		{"empty output", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTestcontainersStartFlake(tt.output); got != tt.want {
				t.Errorf("isTestcontainersStartFlake(...) = %v, want %v\noutput:\n%s", got, tt.want, tt.output)
			}
		})
	}
}

// flakeEchoLine is the signature line the scripted verify commands below emit
// to simulate a testcontainers start-timeout failure. It carries the same
// marker set as the verbatim #972 outputs.
const flakeEchoLine = `wait until ready: mapped port: check target: retries: 9, port: "invalid port", last err: get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/x/json": context deadline exceeded`

// flakeThenPassVerifyCmd writes a verify script that emits the testcontainers
// flake signature and exits non-zero on its FIRST invocation (sentinel file
// outside the worktree), then runs the real module tests on every later
// invocation — the minimal reproduction of a one-shot infra flake.
func flakeThenPassVerifyCmd(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "flaked")
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, "#!/bin/sh\n"+
		"if [ ! -e "+sentinel+" ]; then\n"+
		"  : > "+sentinel+"\n"+
		"  echo '"+flakeEchoLine+"'\n"+
		"  exit 1\n"+
		"fi\n"+
		"cd mod && go test ./...\n")
	return "sh " + script
}

// TestRun_VerifyFixLoop_InfraFlakeRetry_NoBudgetBurn is the #972 fix-loop
// case: the first committed-tree verify fails with the testcontainers
// start-timeout signature; the loop re-runs the verify ONCE in place and it
// passes. The flake must NOT invoke the fix agent and must NOT consume a
// verifyMaxIterations unit, and it must be RECORDED as a
// verify_infra_flake_retry trace event + log line, never swallowed.
func TestRun_VerifyFixLoop_InfraFlakeRetry_NoBudgetBurn(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	// The scope tree is green from the start — the only failure is the flake.
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       flakeThenPassVerifyCmd(t),
		VerifyMaxIterations: 1,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	// ZERO fix-agent invocations: the flake retry is verify-only.
	if invoker.callIdx != 1 {
		t.Errorf("Invoke call count = %d, want 1 (initial only — no fix re-invoke on a flake)", invoker.callIdx)
	}
	if fp.gotArgs == nil {
		t.Error("CommitAndPush should run after the absorbed flake")
	}
	if fpr.gotArgs == nil {
		t.Error("OpenPR should run after the absorbed flake")
	}

	events := readBundleEvents(t, bundlePath)
	verifyRuns, flakeRetries := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case "verify_run":
			verifyRuns++
		case "verify_infra_flake_retry":
			flakeRetries++
		}
	}
	if verifyRuns != 2 {
		t.Errorf("verify_run events = %d, want 2 (flaked + retried)", verifyRuns)
	}
	if flakeRetries != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1 (recorded, not swallowed)", flakeRetries)
	}
	if !strings.Contains(stderr.String(), "verify_infra_flake_retry") {
		t.Errorf("missing verify_infra_flake_retry log line:\n%s", stderr.String())
	}
	// Budget NOT burned: 2 verify attempts against max_iterations=1, passed.
	assertVerifySummary(t, events, "passed", 2, 1)
}

// TestRun_VerifyFixLoop_PersistentInfraFlake_FallsThroughToFixLoop: the flake
// absorb is once-per-stage. A verify that keeps failing with the flake
// signature gets exactly ONE in-place retry, then proceeds into the normal
// fix loop and, on exhaustion, still fails category-A with the captured
// output — a persistent "flake" is treated as a real failure.
func TestRun_VerifyFixLoop_PersistentInfraFlake_FallsThroughToFixLoop(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	// Verify ALWAYS fails with the flake signature.
	dir := t.TempDir()
	script := filepath.Join(dir, "verify.sh")
	mustWrite(t, script, "#!/bin/sh\necho '"+flakeEchoLine+"'\nexit 1\n")

	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "sh " + script,
		VerifyMaxIterations: 1,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("expected category-A terminal demotion after the once-only absorb:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "context deadline exceeded") {
		t.Errorf("failure reason must carry the captured verify output:\n%s", stderr.String())
	}
	// Initial + exactly one fix re-invoke: the absorb spent its one retry,
	// then the normal fix loop ran its single budgeted iteration.
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2 (initial + 1 fix)", invoker.callIdx)
	}
	if fp.gotArgs != nil {
		t.Error("CommitAndPush must not run after a terminal verify-fix exhaustion")
	}

	events := readBundleEvents(t, bundlePath)
	flakeRetries := 0
	for _, ev := range events {
		if ev.Kind == "verify_infra_flake_retry" {
			flakeRetries++
		}
	}
	if flakeRetries != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1 (once-per-stage bound)", flakeRetries)
	}
	// 3 verify attempts: flaked + absorbed retry + post-fix iteration.
	assertVerifySummary(t, events, "failed", 3, 1)
}

// TestRun_VerifyGateCommitted_InfraFlakeRetry_PassProceeds is the #972
// single-shot (#802, max_iterations==0) case: a failed verify whose output
// matches the flake signature is re-run once against the same throwaway
// commit; the retry passes, the gate returns a verified tree, and the push
// proceeds — no ErrCommittedTestsFailed demotion.
func TestRun_VerifyGateCommitted_InfraFlakeRetry_PassProceeds(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       flakeThenPassVerifyCmd(t),
		VerifyMaxIterations: 0, // single-shot committed gate (#802)
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (flake absorbed, no ErrCommittedTestsFailed):\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil {
		t.Error("CommitAndPush should run after the absorbed flake")
	}
	if fpr.gotArgs == nil {
		t.Error("OpenPR should run after the absorbed flake")
	}
	if invoker.callIdx != 1 {
		t.Errorf("Invoke call count = %d, want 1 (single-shot, no re-invoke)", invoker.callIdx)
	}

	events := readBundleEvents(t, bundlePath)
	verifyRuns, flakeRetries := 0, 0
	var sawFailed, sawPassed bool
	for _, ev := range events {
		switch ev.Kind {
		case "verify_run":
			verifyRuns++
			if strings.Contains(string(ev.Data), `"outcome":"failed"`) {
				sawFailed = true
			}
			if strings.Contains(string(ev.Data), `"outcome":"passed"`) {
				sawPassed = true
			}
		case "verify_infra_flake_retry":
			flakeRetries++
		}
	}
	if verifyRuns != 2 {
		t.Errorf("verify_run events = %d, want 2 (flaked + retried)", verifyRuns)
	}
	if flakeRetries != 1 {
		t.Errorf("verify_infra_flake_retry events = %d, want 1", flakeRetries)
	}
	if !sawFailed || !sawPassed {
		t.Errorf("expected one failed and one passed verify_run (failed=%v passed=%v)", sawFailed, sawPassed)
	}
}

// TestRun_VerifyFixLoop_PersistentFixInvokeError_NonBlockingSkip is the #804
// Gap-1/Gap-2 persistent case: EVERY fix re-invocation returns an infra error.
// The loop must (a) hard-bound the fix-Invokes at maxFixInvokeInfraRetries per
// iteration — total Invoke calls EXACTLY 1 (initial) + maxFixInvokeInfraRetries
// — (b) route exhaustion through the non-blocking skip (verify_fix_skipped),
// NOT category-A, (c) let the real push proceed, and (d) emit verify_summary
// EXACTLY ONCE with outcome=skipped.
func TestRun_VerifyFixLoop_PersistentFixInvokeError_NonBlockingSkip(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{
		// idx 0: initial agent (success). idx >= 1: every fix re-invoke errors
		// (errSeq's last entry repeats once the slice is exhausted).
		cannedSeq: []agent.Result{
			{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
			{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		},
		errSeq: []error{nil, agent.ErrAgentFailed},
	}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 1,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	// Non-blocking skip: the stage succeeds and proceeds to the real push.
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (non-blocking skip):\n%s", got, stderr.String())
	}
	// Hard bound pinned: 1 (initial) + maxFixInvokeInfraRetries fix attempts.
	wantCalls := 1 + maxFixInvokeInfraRetries
	if invoker.callIdx != wantCalls {
		t.Errorf("Invoke call count = %d, want %d (initial + maxFixInvokeInfraRetries)", invoker.callIdx, wantCalls)
	}
	// Exhaustion is a non-blocking skip, NEVER a category-A code failure.
	if strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("fix-invoke infra exhaustion must NOT demote to category-A:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "verify_fix_skipped") {
		t.Errorf("expected verify_fix_skipped non-blocking skip:\n%s", stderr.String())
	}
	// The real push proceeds after the skip.
	if fp.gotArgs == nil {
		t.Error("CommitAndPush should run after a non-blocking verify-fix skip")
	}
	if fpr.gotArgs == nil {
		t.Error("OpenPR should run after a non-blocking verify-fix skip")
	}

	events := readBundleEvents(t, bundlePath)
	reinvokeErrs, summaries := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case "verify_fix_reinvoke_error":
			reinvokeErrs++
		case "verify_summary":
			summaries++
		}
	}
	if reinvokeErrs != maxFixInvokeInfraRetries {
		t.Errorf("verify_fix_reinvoke_error events = %d, want %d", reinvokeErrs, maxFixInvokeInfraRetries)
	}
	// verify_summary emitted EXACTLY ONCE on the errored-exit path (Gap-2 fix).
	if summaries != 1 {
		t.Errorf("verify_summary event count = %d, want exactly 1", summaries)
	}
	assertVerifySummary(t, events, "skipped", 1, 1)
}

// TestRun_VerifyFixLoop_InfraExhaustedReinvoke_ReemitsIdenticalDiff is the #870
// advisory-review binding-condition test for the infra-retry-exhausted reinvoke
// path: the loop set reinvoked=true (it reached the reinvoke block) but every
// fix-Invoke errored, so it exits via the non-blocking skip with res.OK still
// true. The post-loop gate (res.OK && implement && !noPR && reinvoked &&
// checkBaseRef) therefore still calls reemitScopedGitDiff. This path is HARMLESS
// and intentional: the failed agent never rewrote the tree, so StageScoped
// produces an identical scope-only diff and last-write-wins == first-write-wins.
// Asserting the re-emitted diff equals the original pins that — a future refactor
// can't silently change re-emit behavior on the infra-exhausted path.
func TestRun_VerifyFixLoop_InfraExhaustedReinvoke_ReemitsIdenticalDiff(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	baseSHA := gitHead(t, repo)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{
		cannedSeq: []agent.Result{
			{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
			{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		},
		errSeq: []error{nil, agent.ErrAgentFailed}, // every fix re-invoke errors
	}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 1,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	args := append(verifyFixRunArgs(repo, bundlePath), "--check-base-ref", baseSHA)
	if got := run(args, &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (non-blocking skip):\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "verify_fix_skipped") {
		t.Fatalf("expected verify_fix_skipped non-blocking skip:\n%s", stderr.String())
	}

	patches := gitDiffPatches(t, readBundleEvents(t, bundlePath))
	if len(patches) != 2 {
		t.Fatalf("git_diff event count = %d, want 2 (original + infra-exhausted re-emit)", len(patches))
	}
	// The agent never rewrote the tree, so the re-emit is byte-identical to the
	// original — last-write-wins is a harmless no-op on this path.
	if patches[0] != patches[1] {
		t.Errorf("infra-exhausted re-emit must equal the original diff (tree unchanged):\nfirst:\n%s\nlast:\n%s", patches[0], patches[1])
	}
}

// TestRun_VerifyGateCommitted_DriftExcludedFailureBlocks is the #802
// single-shot committed-tree gate's core proof: on the implement push path
// with max_iterations==0, the configured verify command runs ONCE against the
// committed SCOPE-ONLY tree (the drift-excluded HEAD), not the agent's dirty
// working tree. A test that passes on the working tree (because an out-of-scope
// seed helper is present) but fails on the committed tree (#780/#776) must block
// as category B (symmetric with #800), name the drift, and NOT push. There is no
// fix re-invoke (single-shot) and no verify_summary (a fix-loop-only event).
func TestRun_VerifyGateCommitted_DriftExcludedFailureBlocks(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetBuggy)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
	// DRIFT helper: makes the WORKING tree green but is dropped from the
	// committed scope-only tree, so the committed tree's test fails.
	mustWrite(t, filepath.Join(repo, "mod", "seed.go"),
		"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")

	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 0, // single-shot committed gate (#802)
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run(verifyFixRunArgs(repo, bundlePath), &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category-B demote on committed-tree verify failure:\n%s", stderr.String())
	}
	// Single-shot: no fix re-invoke.
	if invoker.callIdx != 1 {
		t.Errorf("Invoke call count = %d, want 1 (single-shot committed gate, no re-invoke)", invoker.callIdx)
	}
	// Drift named in the failure reason.
	if !strings.Contains(stderr.String(), "mod/seed.go") {
		t.Errorf("failure reason should name the drift file mod/seed.go:\n%s", stderr.String())
	}
	// Blocked: no push.
	if fp.gotArgs != nil {
		t.Error("CommitAndPush must not run after a committed-tree gate block")
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not run after a committed-tree gate block")
	}
	// Committed-tree execution proof: verify_run carries head_sha; no
	// verify_summary (that is the fix-loop's event, not the single-shot gate).
	events := readBundleEvents(t, bundlePath)
	var sawHeadSHA, sawFailed bool
	for _, ev := range events {
		switch ev.Kind {
		case "verify_run":
			if strings.Contains(string(ev.Data), `"head_sha"`) {
				sawHeadSHA = true
			}
			if strings.Contains(string(ev.Data), `"outcome":"failed"`) {
				sawFailed = true
			}
		case "verify_summary":
			t.Error("single-shot committed gate must not emit verify_summary (fix-loop-only)")
		}
	}
	if !sawHeadSHA {
		t.Error("committed-tree verify_run must carry head_sha")
	}
	if !sawFailed {
		t.Error("committed-tree verify_run must record outcome=failed")
	}
}

// TestRun_VerifyGateCommitted_PassProceeds: when the fix lives in a scope file
// so the committed scope-only tree is green, the single-shot gate passes and the
// push proceeds.
func TestRun_VerifyGateCommitted_PassProceeds(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	// regGetFixed seeds the registry INSIDE the scope file, so the committed
	// scope-only tree passes without any drift helper.
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)

	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "cd mod && go test ./...",
		VerifyMaxIterations: 0,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
			{Path: "mod/reg_test.go", Operation: "create"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fp.gotArgs == nil {
		t.Error("CommitAndPush should run after the committed-tree gate passes")
	}
	if fpr.gotArgs == nil {
		t.Error("OpenPR should run after the committed-tree gate passes")
	}
	if invoker.callIdx != 1 {
		t.Errorf("Invoke call count = %d, want 1 (single-shot, no re-invoke)", invoker.callIdx)
	}
}

// TestRun_VerifyGateCommitted_NothingStaged_NoOp: when no scope-only change is
// present on disk, the throwaway commit has nothing to stage, so the gate is a
// no-op skip (no demotion) and the push proceeds — matching the fix loop's
// no-change handling.
func TestRun_VerifyGateCommitted_NothingStaged_NoOp(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	// No scope-file edits on disk: the base repo's tree is unchanged, so
	// StageScoped stages nothing.
	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)

	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "false", // would fail if it ran against a committed tree
		VerifyMaxIterations: 0,
		ScopeFiles: []upload.ScopeFile{
			{Path: "mod/reg.go", Operation: "modify"},
		},
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK (no-op skip):\n%s", got, stderr.String())
	}
	// The gate emitted a skipped verify_run, never ran the command, never failed.
	events := readBundleEvents(t, bundlePath)
	for _, ev := range events {
		if ev.Kind == "verify_run" && strings.Contains(string(ev.Data), `"outcome":"failed"`) {
			t.Errorf("no-op gate must not run the verify command: %s", ev.Data)
		}
	}
}

// TestRunVerifyGateCommitted_InfraSkipNonBlocking: a PRE-commit infra error (here
// a non-git working dir, so StageScoped fails) is a NON-BLOCKING skip — HEAD was
// never moved, so the helper returns a skipped event + nil error and the real
// push runs its own gate.
func TestRunVerifyGateCommitted_InfraSkipNonBlocking(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cfg := config{
		workingDir: t.TempDir(), // not a git repo → StageScoped errors
		verifyCmd:  "true",
		scopeFiles: []upload.ScopeFile{{Path: "a.txt", Operation: "modify"}},
	}
	evs, tree, err := runVerifyGateCommitted(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("pre-commit infra error must be a non-blocking skip, got %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("event count = %d, want 1 (skip path)", len(evs))
	}
	if !strings.Contains(string(evs[0].Payload), `"outcome":"skipped"`) {
		t.Errorf("expected skipped verify_run on infra error, got %s", evs[0].Payload)
	}
	if tree != "" {
		t.Errorf("skip path must return an empty verified tree (no enforcement), got %q", tree)
	}
}

// TestRunVerifyGateCommitted_PostCommitResetFailureFatal is the #802 approval
// condition: a gitResetSoftHEAD1 failure AFTER a successful throwaway commit is
// FATAL, not a skip. We force it by committing into a repo with no prior commit,
// so the throwaway is the ROOT commit and `git reset --soft HEAD~1` fails (no
// parent). In that state HEAD is left on the throwaway commit, so swallowing the
// error would push the WIP commit into the PR. Distinct from the pre-commit
// infra-skip case, which must stay non-blocking.
func TestRunVerifyGateCommitted_PostCommitResetFailureFatal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, _ := compileGateRepo(t) // git init, no commits yet
	mustWrite(t, filepath.Join(repo, "a.txt"), "hello\n")
	cfg := config{
		workingDir: repo,
		verifyCmd:  "true", // the command itself passes; the reset is what fails
		scopeFiles: []upload.ScopeFile{{Path: "a.txt", Operation: "create"}},
	}
	_, _, err := runVerifyGateCommitted(context.Background(), cfg, io.Discard)
	if err == nil {
		t.Fatal("a post-commit reset failure must be FATAL (hard error), not swallowed to a skip")
	}
	// HEAD must still point at the throwaway commit — the fatal error is exactly
	// what stops openPRAndShipArtifact from stacking the real commit on top.
	if _, herr := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output(); herr != nil {
		t.Fatalf("expected a throwaway commit at HEAD: %v", herr)
	}
}

// TestRunVerifyFixLoop_PostCommitResetFailureFatal is the #816 fix: the fix
// loop's twin of TestRunVerifyGateCommitted_PostCommitResetFailureFatal. A
// gitResetSoftHEAD1 failure AFTER a successful throwaway commit is FATAL (hard
// error), not a non-blocking verify_fix_skipped. We force it by committing into
// a repo with no prior commit, so the throwaway is the ROOT commit and
// `git reset --soft HEAD~1` fails (no parent), leaving HEAD on the throwaway
// commit. Swallowing that to a skip would let openPRAndShipArtifact stack the
// real commit on top and push the WIP commit into the PR.
func TestRunVerifyFixLoop_PostCommitResetFailureFatal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, _ := compileGateRepo(t) // git init, no commits yet
	mustWrite(t, filepath.Join(repo, "a.txt"), "hello\n")
	cfg := config{
		workingDir:          repo,
		verifyCmd:           "true", // the command passes; the reset is what fails
		verifyMaxIterations: 1,
		scopeFiles:          []upload.ScopeFile{{Path: "a.txt", Operation: "create"}},
	}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	res := agent.Result{OK: true}
	var logSink strings.Builder
	_, _, err := runVerifyFixLoop(context.Background(), cfg, invoker, agent.Invocation{}, &res, &logSink)
	if err == nil {
		t.Fatal("a post-commit reset failure must be FATAL (hard error), not a non-blocking skip")
	}
	// HEAD must still point at the throwaway commit — the fatal error is exactly
	// what stops openPRAndShipArtifact from stacking the real commit on top.
	if _, herr := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output(); herr != nil {
		t.Fatalf("expected a throwaway commit at HEAD: %v", herr)
	}
	// The fatal path must NOT route through the non-blocking skip.
	if strings.Contains(logSink.String(), "verify_fix_skipped") {
		t.Errorf("post-commit reset failure must not emit verify_fix_skipped:\n%s", logSink.String())
	}
}

// TestRun_VerifyGateCommitted_Routing_NoPRKeepsWorkingTreeGate: a --no-pr
// implement run has no committed tree to gate, so it keeps the #441 single-shot
// WORKING-TREE gate (category A), NOT the committed gate.
func TestRun_VerifyGateCommitted_Routing_NoPRKeepsWorkingTreeGate(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "implement",
		Prompt:              "implement",
		PromptHash:          "h",
		VerifyCommand:       "false",
		VerifyMaxIterations: 0,
		ScopeFiles:          []upload.ScopeFile{{Path: "mod/reg.go", Operation: "modify"}},
	}
	withFakeUploader(t, fu)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run([]string{
		"--run-id", verifyFixRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", verifyFixStageID,
		"--no-pr",
		"--fetch-prompt", "--upload-trace",
		"--bundle-out", bundlePath,
	}, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("--no-pr must keep the working-tree gate (category A):\n%s", stderr.String())
	}
	// Working-tree gate carries no head_sha and emits no verify_summary.
	for _, ev := range readBundleEvents(t, bundlePath) {
		if ev.Kind == "verify_run" && strings.Contains(string(ev.Data), `"head_sha"`) {
			t.Error("working-tree gate must not carry head_sha")
		}
		if ev.Kind == "verify_summary" {
			t.Error("working-tree gate must not emit verify_summary")
		}
	}
}

// TestRun_VerifyGateCommitted_Routing_PlanStageKeepsWorkingTreeGate: a plan
// stage has no committed tree to gate, so it keeps the working-tree gate
// (category A), NOT the committed gate.
func TestRun_VerifyGateCommitted_Routing_PlanStageKeepsWorkingTreeGate(t *testing.T) {
	invoker := &fakeInvoker{canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:             verifyFixStageID,
		StageType:           "plan",
		Prompt:              "plan",
		PromptHash:          "h",
		VerifyCommand:       "false",
		VerifyMaxIterations: 0,
	}
	withFakeUploader(t, fu)

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	got := run([]string{
		"--run-id", verifyFixRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", verifyFixStageID,
		"--fetch-prompt", "--upload-trace",
		"--bundle-out", bundlePath,
	}, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("plan stage must keep the working-tree gate (category A):\n%s", stderr.String())
	}
	for _, ev := range readBundleEvents(t, bundlePath) {
		if ev.Kind == "verify_summary" {
			t.Error("plan-stage working-tree gate must not emit verify_summary")
		}
	}
}

// Routing case maxIterations>0 (implement push) → runVerifyFixLoop, NOT the
// single-shot committed gate, is covered by TestRun_VerifyFixLoop_* above: those
// assert a verify_summary event, which ONLY the fix loop emits. The single-shot
// committed gate emits no verify_summary, so the partition is unambiguous.

// TestSemverLT covers the semverLT helper including numeric comparison
// (not string comparison) so v0.9.0 < v0.10.0 is handled correctly.
func TestSemverLT(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0", false},
		{"v0.9.0", "v0.10.0", true},  // numeric, not string
		{"v0.10.0", "v0.9.0", false}, // numeric, not string
		{"v1.0.0", "v0.9.9", false},
		{"v0.4.0", "v0.5.0", true},
		{"v0.5.0", "v0.4.0", false},
		// dev builds never trigger skew
		{"dev", "v0.5.0", false},
		{"v0.4.0", "dev", false},
		{"dev", "dev", false},
		// unparseable
		{"not-semver", "v0.1.0", false},
	}
	for _, tc := range cases {
		t.Run(tc.a+"<"+tc.b, func(t *testing.T) {
			if got := semverLT(tc.a, tc.b); got != tc.want {
				t.Errorf("semverLT(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestRun_VersionSubcommand verifies that 'fishhawk-runner version' emits
// JSON with version and plan_schema_hash to STDOUT (not logSink), then
// exits 0. stdout is the right stream because `fishhawk doctor` parses
// the output via exec.Command().Output() which captures stdout only.
func TestRun_VersionSubcommand(t *testing.T) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	origStdout := os.Stdout
	os.Stdout = stdoutW
	t.Cleanup(func() { os.Stdout = origStdout })

	var logSink strings.Builder
	got := run([]string{"version"}, &logSink)
	if err := stdoutW.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	stdoutBytes, err := io.ReadAll(stdoutR)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if got != exitOK {
		t.Fatalf("run version = %d, want exitOK; stdout: %s; logSink: %s", got, stdoutBytes, logSink.String())
	}
	if logSink.Len() != 0 {
		t.Errorf("logSink must be empty, got: %s", logSink.String())
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(stdoutBytes))), &body); err != nil {
		t.Fatalf("version output not JSON: %v\n%s", err, stdoutBytes)
	}
	if body["version"] == "" {
		t.Error("version field must not be empty")
	}
	if len(body["plan_schema_hash"]) != 64 {
		t.Errorf("plan_schema_hash = %q, want 64-char hex", body["plan_schema_hash"])
	}
}

// TestRun_VersionSkewDetected verifies that run() returns exitVersionSkew
// when the backend's min_runner_version is newer than the runner's version.
func TestRun_VersionSkewDetected(t *testing.T) {
	// Inject a known parseable version so semverLT fires deterministically.
	origVersion := runnerVersion
	runnerVersion = func() string { return "v0.4.0" }
	t.Cleanup(func() { runnerVersion = origVersion })

	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:          "22222222-3333-4444-5555-666666666666",
		StageType:        "plan",
		Prompt:           "test prompt",
		PromptHash:       "deadbeef",
		MinRunnerVersion: "v0.5.0",
	}
	withFakeUploader(t, fu)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "plan",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
	}, &stderr)
	if got != exitVersionSkew {
		t.Errorf("run = %d, want exitVersionSkew (%d); stderr:\n%s", got, exitVersionSkew, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"version_skew_detected"`) {
		t.Errorf("missing version_skew_detected log:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"min_required":"v0.5.0"`) {
		t.Errorf("missing min_required in log:\n%s", stderr.String())
	}
}

// selfRetryPromptResp is the promptResp used by ADR-023 self-retry tests.
// agentSelfRetry:true, budget=1 (maxRetriesSnapshot=1, retryAttempt=0).
func selfRetryPromptResp(stageID string) *upload.FetchedPrompt {
	return &upload.FetchedPrompt{
		StageID:            stageID,
		StageType:          "plan",
		Prompt:             "retry test prompt",
		PromptHash:         "deadbeef",
		AgentSelfRetry:     true,
		MaxRetriesSnapshot: 1,
		RetryAttempt:       0,
	}
}

const selfRetryStageID = "22222222-3333-4444-5555-666666666666"
const selfRetryRunID = "11111111-2222-3333-4444-555555555555"

// selfRetryArgs returns the common run() flags for the self-retry tests.
func selfRetryArgs() []string {
	return []string{
		"--run-id", selfRetryRunID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "w", "--stage", "s",
		"--stage-id", selfRetryStageID,
		"--fetch-prompt",
	}
}

// TestRun_SelfRetry_CategoryA_ThenSuccess verifies the happy path:
// a category-A failure on the first invoke, RetryStage succeeds,
// the second invoke succeeds → exit 0, RetryStage called exactly once.
func TestRun_SelfRetry_CategoryA_ThenSuccess(t *testing.T) {
	invoker := &fakeInvoker{
		cannedSeq: []agent.Result{
			{OK: false, FailureCategory: "A", FailureReason: "agent crash",
				Events: []agent.Event{{Kind: "invocation_start"}}},
			{OK: true, TokensUsed: 5,
				Events: []agent.Event{{Kind: "invocation_start"}}},
		},
		errSeq: []error{agent.ErrAgentFailed, nil},
	}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	fu.promptResp = selfRetryPromptResp(selfRetryStageID)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run(selfRetryArgs(), &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if len(fu.gotRetryArgs) != 1 {
		t.Errorf("RetryStage calls = %d, want 1", len(fu.gotRetryArgs))
	}
	if fu.gotRetryArgs[0].StageID != selfRetryStageID {
		t.Errorf("RetryStage StageID = %q, want %q", fu.gotRetryArgs[0].StageID, selfRetryStageID)
	}
	if !strings.Contains(stderr.String(), `"event":"stage_self_retry"`) {
		t.Errorf("missing stage_self_retry log:\n%s", stderr.String())
	}
	if invoker.callIdx != 2 {
		t.Errorf("Invoke call count = %d, want 2", invoker.callIdx)
	}
}

// TestRun_SelfRetry_CategoryB_NoRetry verifies that category-B failures
// do not trigger a self-retry even when agent_self_retry is true.
func TestRun_SelfRetry_CategoryB_NoRetry(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "B", FailureReason: "constraint violated"},
		returnErr: nil,
	})
	fu := newFakeUploader(t)
	fu.promptResp = selfRetryPromptResp(selfRetryStageID)
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run(selfRetryArgs(), &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if len(fu.gotRetryArgs) != 0 {
		t.Errorf("RetryStage should not be called for category-B failure; got %d calls", len(fu.gotRetryArgs))
	}
}

// TestRun_SelfRetry_ZeroBudget_NoRetry verifies that a zero retry budget
// (maxRetriesSnapshot == retryAttempt) prevents self-retry even on A failures.
func TestRun_SelfRetry_ZeroBudget_NoRetry(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: agent.ErrAgentFailed,
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:            selfRetryStageID,
		StageType:          "plan",
		Prompt:             "test",
		PromptHash:         "h",
		AgentSelfRetry:     true,
		MaxRetriesSnapshot: 1,
		RetryAttempt:       1, // budget = 0
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run(selfRetryArgs(), &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if len(fu.gotRetryArgs) != 0 {
		t.Errorf("RetryStage should not be called when budget is exhausted; got %d calls", len(fu.gotRetryArgs))
	}
}

// TestRun_SelfRetry_Disabled_NoRetry verifies that agent_self_retry:false
// prevents self-retry on category-A failures.
func TestRun_SelfRetry_Disabled_NoRetry(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: agent.ErrAgentFailed,
	})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:            selfRetryStageID,
		StageType:          "plan",
		Prompt:             "test",
		PromptHash:         "h",
		AgentSelfRetry:     false, // opt-out
		MaxRetriesSnapshot: 1,
		RetryAttempt:       0,
	}
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run(selfRetryArgs(), &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if len(fu.gotRetryArgs) != 0 {
		t.Errorf("RetryStage should not be called when agent_self_retry is false; got %d calls", len(fu.gotRetryArgs))
	}
}

// TestRun_SelfRetry_RetryStage422_ExitsWithOriginalFailure verifies that
// when RetryStage returns ErrRetryNotApplicable (422) the runner does not
// loop and exits with the original category-A failure.
func TestRun_SelfRetry_RetryStage422_ExitsWithOriginalFailure(t *testing.T) {
	withFakeInvoker(t, &fakeInvoker{
		canned:    agent.Result{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
		returnErr: agent.ErrAgentFailed,
	})
	fu := newFakeUploader(t)
	fu.promptResp = selfRetryPromptResp(selfRetryStageID)
	fu.retryStageErr = upload.ErrRetryNotApplicable
	withFakeUploader(t, fu)

	var stderr strings.Builder
	got := run(selfRetryArgs(), &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	// RetryStage was called once (the 422 path), then the loop broke.
	if len(fu.gotRetryArgs) != 1 {
		t.Errorf("RetryStage calls = %d, want 1 (called but not looped)", len(fu.gotRetryArgs))
	}
	if !strings.Contains(stderr.String(), `"event":"retry_stage_failed"`) {
		t.Errorf("missing retry_stage_failed log:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `"category":"A"`) {
		t.Errorf("original category-A must be preserved:\n%s", stderr.String())
	}
}

// --- #728 implement-stage compile gate -------------------------------

// compileGateRepo inits a throwaway git repo with a commit-able identity
// and signing disabled, returning the repo path and a bound runGit.
func compileGateRepo(t *testing.T) (string, func(args ...string)) {
	t.Helper()
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "--initial-branch=main")
	runGit("config", "user.name", "init")
	runGit("config", "user.email", "init@example.com")
	runGit("config", "commit.gpgsign", "false")
	return repo, runGit
}

func gitHead(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestVerifyCommittedTreeCompiles_NoDriftFastPath: with no excluded
// files, nothing build-required could have been dropped, so the gate
// returns nil immediately without touching git or the toolchain.
func TestVerifyCommittedTreeCompiles_NoDriftFastPath(t *testing.T) {
	if err := verifyCommittedTreeCompiles(context.Background(), t.TempDir(), "deadbeef", nil, nil, io.Discard); err != nil {
		t.Errorf("no-drift fast path should return nil, got %v", err)
	}
}

// TestVerifyCommittedTreeCompiles_NonGoRepoFastPath: a repo with no
// go.work at its root is not a Go workspace to vet, so the gate skips
// (returns nil) even when drift is present.
func TestVerifyCommittedTreeCompiles_NonGoRepoFastPath(t *testing.T) {
	if err := verifyCommittedTreeCompiles(context.Background(), t.TempDir(), "deadbeef", []string{"x.go"}, []string{"x.go"}, io.Discard); err != nil {
		t.Errorf("non-Go-repo fast path should return nil, got %v", err)
	}
}

// TestVerifyCommittedTreeCompiles_CompileFailureBlocks is the end-to-end
// seam for #728: it crosses the commit→worktree→vet boundaries. A
// scope-only committed tree drops a build-required conformance method,
// so the committed tree fails `go vet` (the assertion lives in a test
// file, which `go build` would skip but `go vet` typechecks). The
// working tree carries the fix as drift on disk and compiles — proving
// the gate builds the COMMITTED tree, not the working tree. The test is
// hermetic: the module has no external dependencies.
func TestVerifyCommittedTreeCompiles_CompileFailureBlocks(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	// impl does NOT implement Doer; the failing assertion is in a _test.go
	// file so only `go vet` (not `go build`) catches it.
	mustWrite(t, filepath.Join(repo, "mod", "doer.go"),
		"package mod\n\ntype Doer interface{ Do() }\n\ntype impl struct{}\n")
	mustWrite(t, filepath.Join(repo, "mod", "doer_test.go"),
		"package mod\n\nvar _ Doer = impl{}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "scope-only commit missing the Do() method")
	head := gitHead(t, repo)

	// Working-tree drift: add the conformance method on disk (uncommitted).
	// The working tree now compiles, but the committed tree does not.
	mustWrite(t, filepath.Join(repo, "mod", "doer.go"),
		"package mod\n\ntype Doer interface{ Do() }\n\ntype impl struct{}\n\nfunc (impl) Do() {}\n")
	vet := exec.Command("go", "vet", "./...")
	vet.Dir = filepath.Join(repo, "mod")
	if out, err := vet.CombinedOutput(); err != nil {
		t.Fatalf("working tree should compile, but go vet failed: %v\n%s", err, out)
	}

	var log bytes.Buffer
	err := verifyCommittedTreeCompiles(context.Background(), repo, head, []string{"mod/doer.go"}, []string{"mod/doer.go"}, &log)
	if !errors.Is(err, gitops.ErrCommitWouldNotCompile) {
		t.Fatalf("err = %v, want ErrCommitWouldNotCompile\nlog: %s", err, log.String())
	}
	if !strings.Contains(err.Error(), "mod/doer.go") {
		t.Errorf("error should name the drift file mod/doer.go: %v", err)
	}
}

// TestVerifyCommittedTreeCompiles_UnknownFieldDiagnosticBlocks is the
// #959 end-to-end seam: the exact misclassified run-07bce059 class — an
// unknown-field struct-literal typecheck error in a _test.go file,
// whose message is outside compileDiagnosticMarkers — must block via
// goDiagnosticBlocks. It runs the real `go vet` against a broken
// committed tree, so it pins the live `vet: file:line:col:` output
// shape rather than a transcript assumption; if the toolchain's output
// shape ever stops matching goDiagnosticLine, this fails.
func TestVerifyCommittedTreeCompiles_UnknownFieldDiagnosticBlocks(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	// The committed Config lacks the Retries field the test sets — the
	// `unknown field ... in struct literal of type ...` diagnostic, in a
	// _test.go file so only `go vet` (not `go build`) catches it.
	mustWrite(t, filepath.Join(repo, "mod", "config.go"),
		"package mod\n\ntype Config struct{ Name string }\n")
	mustWrite(t, filepath.Join(repo, "mod", "config_test.go"),
		"package mod\n\nvar _ = Config{Retries: 3}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "scope-only commit missing the Retries field")
	head := gitHead(t, repo)

	// Working-tree drift: add the field on disk (uncommitted), so the
	// working tree compiles while the committed tree does not.
	mustWrite(t, filepath.Join(repo, "mod", "config.go"),
		"package mod\n\ntype Config struct {\n\tName    string\n\tRetries int\n}\n")
	vet := exec.Command("go", "vet", "./...")
	vet.Dir = filepath.Join(repo, "mod")
	if out, err := vet.CombinedOutput(); err != nil {
		t.Fatalf("working tree should compile, but go vet failed: %v\n%s", err, out)
	}

	var log bytes.Buffer
	err := verifyCommittedTreeCompiles(context.Background(), repo, head, []string{"mod/config.go"}, []string{"mod/config.go"}, &log)
	if !errors.Is(err, gitops.ErrCommitWouldNotCompile) {
		t.Fatalf("err = %v, want ErrCommitWouldNotCompile (unknown-field diagnostic must block, #959)\nlog: %s", err, log.String())
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("error should carry the live vet diagnostic: %v", err)
	}
}

// TestVerifyCommittedTreeCompiles_DepResolutionFailureSkips: a `go vet`
// non-zero exit caused by an unresolvable dependency (offline) is NOT a
// compile/typecheck diagnostic, so it must skip (non-blocking) rather
// than block. Hermetic: GOPROXY=off forces the failure to surface as a
// dependency-resolution error with no network.
func TestVerifyCommittedTreeCompiles_DepResolutionFailureSkips(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(repo, "mod", "use.go"),
		"package mod\n\nimport _ \"example.com/totally/absent/pkg\"\n")
	runGit("add", "-A")
	runGit("commit", "-m", "imports an unavailable dependency")
	head := gitHead(t, repo)

	var log bytes.Buffer
	if err := verifyCommittedTreeCompiles(context.Background(), repo, head, []string{"mod/use.go"}, []string{"mod/use.go"}, &log); err != nil {
		t.Fatalf("dep-resolution failure must NOT block, got %v\nlog: %s", err, log.String())
	}
	if !strings.Contains(log.String(), "compile_gate_skipped") {
		t.Errorf("expected compile_gate_skipped log, got: %s", log.String())
	}
}

// TestLooksLikeCompileError covers the positive-allowlist classifier
// that separates a genuine typecheck diagnostic (block) from a
// dependency-resolution failure or vet-analyzer finding (skip).
func TestLooksLikeCompileError(t *testing.T) {
	block := []string{
		"fake does not implement R (missing method Foo)",
		"cannot use fake{} (variable of type fake) as R value",
		"./x.go:3:9: undefined: Bar",
		// #774: the selector-on-type typecheck form (the exact #762 child D
		// diagnostic) — "undefined:" does not match this, so the
		// "undefined (" / "has no field or method" markers must.
		"cmd/fishhawk-mcp/fixup_test.go:24:5: fb.fixupResp undefined (type *fakeBackend has no field or method fixupResp)",
		"./x.go:5:9: int is not a type",
		"./x.go:7:9: cannot convert s (variable of type string) to type int",
	}
	for _, s := range block {
		if !looksLikeCompileError(s) {
			t.Errorf("looksLikeCompileError(%q) = false, want true", s)
		}
	}
	skip := []string{
		"no required module provides package example.com/x; to add it: go get example.com/x",
		"cannot find module providing package example.com/y",
		"Printf format %d has arg s of wrong type string",
		"",
	}
	for _, s := range skip {
		if looksLikeCompileError(s) {
			t.Errorf("looksLikeCompileError(%q) = true, want false", s)
		}
	}
	// #959: messages outside the marker list but carrying a clean
	// file:line:col diagnostic line block via the combined gate condition
	// (looksLikeCompileError backstop OR goDiagnosticBlocks primary) — the
	// exact diagnostic the marker allowlist missed on run 07bce059.
	combined := []string{
		"vet: internal/server/trace_test.go:2381:3: unknown field PlanReviewer in struct literal of type Config",
	}
	for _, s := range combined {
		if !looksLikeCompileError(s) && !goDiagnosticBlocks(s) {
			t.Errorf("combined classifier (looksLikeCompileError || goDiagnosticBlocks)(%q) = false, want true", s)
		}
	}
}

// goTestBuildFailedOutput959 is the verbatim `go test` output from run
// 07bce059 (#959): a clean compiler diagnostic plus `FAIL <pkg> [build
// failed]`. The test phase previously skipped this as infra
// (test_nonzero_non_failure); it is a definitive compile failure of the
// tree being pushed and must block.
const goTestBuildFailedOutput959 = `# github.com/kuhlman-labs/fishhawk/backend/internal/server [github.com/kuhlman-labs/fishhawk/backend/internal/server.test]
internal/server/trace_test.go:2408:74: cannot use 1 (untyped int constant) as []reviewerInvocation value in struct literal
FAIL	github.com/kuhlman-labs/fishhawk/backend/internal/server [build failed]
FAIL`

// TestGoDiagnosticBlocks covers the #959 primary classifier: block on
// any clean Go compiler/typecheck diagnostic line (file:line:col,
// optional `vet: ` prefix) unless every such line is a recognized
// dependency-resolution form. The two verbatim run-07bce059 outputs are
// pinned as blocking; dep-resolution forms (which print in the SAME
// file:line:col shape under GOPROXY=off), bare [build failed]/[setup
// failed] with no diagnostic, go-test log lines (file:line:, no
// column), and exec/OOM noise must not block.
func TestGoDiagnosticBlocks(t *testing.T) {
	block := []struct{ name, output string }{
		{"verbatim #959 vet unknown-field diagnostic",
			"vet: internal/server/trace_test.go:2381:3: unknown field PlanReviewer in struct literal of type Config"},
		{"verbatim #959 go test build-failed with diagnostic", goTestBuildFailedOutput959},
		{"bare diagnostic without vet prefix", "./x.go:3:9: undefined: Bar"},
		// Deliberate #959 behavior change: a vet ANALYZER finding (plain
		// file:line:col under a `# pkg` header) now blocks — CI's
		// golangci-lint bundles govet so the PR would red-line anyway, and
		// a block routes to verify_fix_reinvoke, not a dead stage.
		{"vet analyzer finding", "# example.com/moda\n./a.go:5:14: fmt.Printf format %d has arg \"x\" of wrong type string"},
	}
	for _, tt := range block {
		t.Run(tt.name, func(t *testing.T) {
			if !goDiagnosticBlocks(tt.output) {
				t.Errorf("goDiagnosticBlocks(...) = false, want true\noutput:\n%s", tt.output)
			}
		})
	}
	skip := []struct{ name, output string }{
		{"dep resolution in diagnostic shape (GOPROXY=off)",
			"use_test.go:6:2: no required module provides package example.com/totally/absent/pkg; to add it:\n\tgo get example.com/totally/absent/pkg"},
		{"missing go.sum entry in diagnostic shape",
			"x.go:5:2: missing go.sum entry for module providing package example.com/y"},
		{"bare build-failed with no diagnostic line", "FAIL\texample.com/mod [build failed]\nFAIL"},
		{"setup failed", "FAIL\texample.com/mod [setup failed]\nFAIL"},
		{"go test log line has no column", "--- FAIL: TestGet (0.00s)\n    reg_test.go:7: Get(x) = 0, want 42\nFAIL"},
		{"exec noise", "fork/exec /usr/local/go/pkg/tool/darwin_arm64/vet: cannot allocate memory"},
		{"empty output", ""},
	}
	for _, tt := range skip {
		t.Run(tt.name, func(t *testing.T) {
			if goDiagnosticBlocks(tt.output) {
				t.Errorf("goDiagnosticBlocks(...) = true, want false\noutput:\n%s", tt.output)
			}
		})
	}
}

// TestGoDiagnosticBlocks_FlakeAbsorbOrdering pins the #972/#959
// classification ordering (approval condition): a testcontainers
// container-start-timeout output must route to the infra-flake absorb
// (isTestcontainersStartFlake, checked FIRST in both committed-tree
// verify gates), never to the #959 failure classification — the flake
// outputs carry go-test log lines (file:line:, no column), so
// goDiagnosticBlocks must not match them.
func TestGoDiagnosticBlocks_FlakeAbsorbOrdering(t *testing.T) {
	for _, tt := range []struct{ name, output string }{
		{"verbatim #972 approval-package failure", flakeOutputApproval},
		{"verbatim #972 audit-package failure", flakeOutputAudit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if !isTestcontainersStartFlake(tt.output) {
				t.Errorf("isTestcontainersStartFlake(...) = false, want true (flake retry must claim this output)")
			}
			if goDiagnosticBlocks(tt.output) {
				t.Errorf("goDiagnosticBlocks(...) = true, want false (testcontainers flake must route to the #972 absorb, not the #959 classification)")
			}
		})
	}
}

// TestRun_ImplementStage_CompileGateFailure_CategoryB confirms the #728
// failure mode (documented by #742): when the pre-push compile gate
// returns ErrCommitWouldNotCompile, the implement stage must surface as
// FAILED with category B (re-scope/re-plan), exit non-zero, and abort
// BEFORE opening a PR — no non-compiling PR, no null-PR "succeeded"
// zombie. CommitAndPush returns the sentinel exactly as the real
// gitops.Pusher does when its VerifyCommit hook fails.
func TestRun_ImplementStage_CompileGateFailure_CategoryB(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:    "22222222-3333-4444-5555-666666666666",
		StageType:  "implement",
		Prompt:     "implement",
		PromptHash: "h",
	}
	withFakeUploader(t, fu)
	fp := &fakePusher{err: gitops.ErrCommitWouldNotCompile}
	fpr := &fakePROpener{}
	withFakeGitOps(t, fp, fpr)

	var stderr strings.Builder
	got := run([]string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt", "--upload-trace",
	}, &stderr)
	if got != exitFailure {
		t.Errorf("run = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), `"category":"B"`) {
		t.Errorf("expected category-B on compile-gate failure, got:\n%s", stderr.String())
	}
	// Gate is wired on the standalone PR-opening path.
	if fp.gotArgs == nil || fp.gotArgs.VerifyCommit == nil {
		t.Error("VerifyCommit hook should be set on the standalone implement path")
	}
	// Aborted pre-PR: no non-compiling PR is opened.
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not be called after a compile-gate failure")
	}
	// #742: the category-B compile-gate failure is reported to the backend
	// via the /pull-request FAILURE path so the gated implement stage lands
	// `failed` instead of hanging in running. It must be the failure report
	// (outcome=failed / category=B), NOT a success PR artifact — no PR was
	// opened, so the report carries no artifact body.
	if fu.gotPRArgs == nil {
		t.Fatal("ShipPullRequest (failure report) must be called after a compile-gate failure (#742)")
	}
	if fu.gotPRArgs.Outcome != "failed" || fu.gotPRArgs.Category != "B" {
		t.Errorf("ShipPullRequest failure report = {outcome:%q, category:%q}, want {failed, B}",
			fu.gotPRArgs.Outcome, fu.gotPRArgs.Category)
	}
	if len(fu.gotPRArgs.Body) != 0 {
		t.Errorf("failure report must not carry a success PR artifact body, got %d bytes", len(fu.gotPRArgs.Body))
	}
}

// TestVerifyCommittedTreeCompiles_NonCompileVetFailureDoesNotMaskLater
// guards the per-module loop continuation: a non-blocking `go vet`
// failure in an EARLIER module (here a dependency-resolution failure —
// since #959 the only still-skipping diagnostic-shaped class; an
// analyzer finding now blocks) must not abandon the gate — a genuine
// build-required-drift compile error in a LATER module must still be
// caught. (Regression: an earlier `return nil` on the non-blocking skip
// would have returned before reaching modb.)
func TestVerifyCommittedTreeCompiles_NonCompileVetFailureDoesNotMaskLater(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GOPROXY", "off")

	repo, runGit := compileGateRepo(t)
	// moda is iterated first (go work edit -json preserves use order).
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./moda\nuse ./modb\n")
	for _, m := range []string{"moda", "modb"} {
		if err := os.MkdirAll(filepath.Join(repo, m), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(repo, m, "go.mod"), "module example.com/"+m+"\n\ngo 1.21\n")
	}
	// moda: imports an unavailable dependency (GOPROXY=off), so `go vet`
	// exits non-zero with a dep-resolution diagnostic → non-blocking skip.
	mustWrite(t, filepath.Join(repo, "moda", "a.go"),
		"package moda\n\nimport _ \"example.com/totally/absent/pkg\"\n")
	// modb: the interface-ripple compile error in a _test.go file.
	mustWrite(t, filepath.Join(repo, "modb", "doer.go"),
		"package modb\n\ntype Doer interface{ Do() }\n\ntype impl struct{}\n")
	mustWrite(t, filepath.Join(repo, "modb", "doer_test.go"),
		"package modb\n\nvar _ Doer = impl{}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "moda dep-resolution failure; modb missing Do()")
	head := gitHead(t, repo)

	var log bytes.Buffer
	err := verifyCommittedTreeCompiles(context.Background(), repo, head, []string{"modb/doer.go"}, []string{"modb/doer.go"}, &log)
	if !errors.Is(err, gitops.ErrCommitWouldNotCompile) {
		t.Fatalf("err = %v, want ErrCommitWouldNotCompile (modb compile error must be caught despite moda's dep-resolution skip)\nlog: %s", err, log.String())
	}
	// moda's dep-resolution failure should have been logged as a skip.
	if !strings.Contains(log.String(), "vet_nonzero_non_compile") {
		t.Errorf("expected moda's dep-resolution vet failure to log a skip; log: %s", log.String())
	}
}

// TestVerifyGate_StripsRunnerCredsFromSubprocess is the ADR-029 #650 item 4
// seam test for the working-tree verify gate (#618 cross-boundary rule): it
// plants a sentinel runner secret in the process env, drives the REAL
// runVerifyGate exec site with an env-printing verifyCmd, and asserts the
// sentinel is absent from the captured subprocess output while PATH survives.
// A correct sanitizeEnv with a call site that forgot `cmd.Env =` would leak
// here — a unit test on sanitizeEnv alone would pass while the seam leaks.
func TestVerifyGate_StripsRunnerCredsFromSubprocess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	const canary = "leak-canary-verifygate"
	t.Setenv("FISHHAWK_GITHUB_TOKEN", canary)

	ev, err := runVerifyGate(context.Background(), config{verifyCmd: "env"}, io.Discard)
	if err != nil {
		t.Fatalf("env should exit 0, got %v\npayload: %s", err, ev.Payload)
	}
	out := string(ev.Payload)
	if strings.Contains(out, canary) {
		t.Errorf("runVerifyGate leaked the runner secret into the gate subprocess env:\n%s", out)
	}
	if !strings.Contains(out, "PATH=") {
		t.Errorf("PATH must be preserved in the gate env; payload:\n%s", out)
	}
}

// TestVerifyCommittedTree_StripsRunnerCredsFromSubprocess is the structural
// twin of TestVerifyGate_StripsRunnerCredsFromSubprocess for the committed-tree
// verify exec site (runVerifyCommittedTree): the same canary-absent seam
// assertion, so a forgotten `cmd.Env =` at THAT site also fails the suite.
func TestVerifyCommittedTree_StripsRunnerCredsFromSubprocess(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	const canary = "leak-canary-committedtree"
	t.Setenv("FISHHAWK_GITHUB_TOKEN", canary)

	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "f.txt"), "seed\n")
	runGit("add", "-A")
	runGit("commit", "-m", "seed commit")
	head := gitHead(t, repo)

	_, out, _ := runVerifyCommittedTree(context.Background(), "env", repo, head, time.Minute)
	if strings.Contains(out, canary) {
		t.Errorf("runVerifyCommittedTree leaked the runner secret into the gate subprocess env:\n%s", out)
	}
	if !strings.Contains(out, "PATH=") {
		t.Errorf("PATH must be preserved in the gate env; output:\n%s", out)
	}
}

// TestVerifyCommittedTreeCompiles_GateSubprocessEnvStripped covers the
// go-toolchain gate exec sites (go work edit / go vet / go test inside
// verifyCommittedTreeCompiles). It plants a sentinel runner secret, then
// commits a probe test that FAILS if `go test` can see FISHHAWK_GITHUB_TOKEN.
// With the token stripped from the gate env the probe passes and the gate
// returns nil; a forgotten `cmd.Env =` at the testCmd site would let the probe
// see the token, fail, and red-line the gate — so this seam test catches a
// leak at the committed-tree compile/test sites.
func TestVerifyCommittedTreeCompiles_GateSubprocessEnvStripped(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	const canary = "leak-canary-committedcompile"
	t.Setenv("FISHHAWK_GITHUB_TOKEN", canary)

	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(repo, "mod", "probe.go"),
		"package mod\n\nfunc Noop() {}\n")
	// The probe test fails iff the gate subprocess inherited the runner token.
	mustWrite(t, filepath.Join(repo, "mod", "probe_test.go"),
		"package mod\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n"+
			"func TestNoRunnerToken(t *testing.T) {\n"+
			"\tif v := os.Getenv(\"FISHHAWK_GITHUB_TOKEN\"); v != \"\" {\n"+
			"\t\tt.Fatalf(\"gate subprocess saw runner token: %q\", v)\n\t}\n}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "env-leak probe")
	head := gitHead(t, repo)

	var log bytes.Buffer
	// Non-empty drift so the gate runs past the no-drift fast path; scope names
	// the touched package so the test phase actually runs probe_test.go.
	err := verifyCommittedTreeCompiles(context.Background(), repo, head,
		[]string{"README.md"}, []string{"mod/probe.go", "mod/probe_test.go"}, &log)
	if err != nil {
		t.Fatalf("gate must pass with the runner token stripped from the gate env, got %v\nlog: %s", err, log.String())
	}
	if strings.Contains(log.String(), canary) {
		t.Errorf("canary leaked into gate output:\n%s", log.String())
	}
}

// TestVerifyCommittedTreeTests_DriftExcludedFailureBlocks is the #800/#780
// proof: a committed tree that COMPILES (go vet passes) but has a FAILING
// test in a touched package because a helper file was excluded as scope
// drift (present only in the working tree). seed.go seeds a package var an
// init reads; dropping it from the commit leaves Get returning the zero
// value, so the committed test fails while the working tree (with seed.go)
// would pass. The gate must return ErrCommittedTestsFailed naming the
// failing package — the class the vet-only gate (#728) could not catch.
func TestVerifyCommittedTreeTests_DriftExcludedFailureBlocks(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"),
		"package mod\n\nvar registry = map[string]int{}\n\nfunc Get(k string) int { return registry[k] }\n")
	mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"),
		"package mod\n\nimport \"testing\"\n\nfunc TestGet(t *testing.T) {\n\tif Get(\"x\") != 42 {\n\t\tt.Fatalf(\"Get(x) = %d, want 42\", Get(\"x\"))\n\t}\n}\n")
	// reg.go + reg_test.go compile and the test passes ONLY when seed.go is
	// present. Commit WITHOUT seed.go (it is the scope drift).
	runGit("add", "-A")
	runGit("commit", "-m", "scope-only commit missing the seed helper")
	head := gitHead(t, repo)

	// Working-tree drift: add the seed helper on disk (uncommitted). The
	// working tree's tests now pass; the committed tree's do not.
	mustWrite(t, filepath.Join(repo, "mod", "seed.go"),
		"package mod\n\nfunc init() { registry[\"x\"] = 42 }\n")

	var log bytes.Buffer
	err := verifyCommittedTreeCompiles(context.Background(), repo, head,
		[]string{"mod/seed.go"}, []string{"mod/reg.go", "mod/reg_test.go"}, &log)
	if !errors.Is(err, gitops.ErrCommittedTestsFailed) {
		t.Fatalf("err = %v, want ErrCommittedTestsFailed\nlog: %s", err, log.String())
	}
	if !strings.Contains(err.Error(), "mod/seed.go") {
		t.Errorf("error should name the drift file mod/seed.go: %v", err)
	}
}

// TestVerifyCommittedTreeTests_GreenWithDriftPasses: a fully-green
// committed tree (the touched package's tests pass) with drift present
// must pass the gate (nil) — the test phase runs but finds no failure.
func TestVerifyCommittedTreeTests_GreenWithDriftPasses(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(repo, "mod", "add.go"),
		"package mod\n\nfunc Add(a, b int) int { return a + b }\n")
	mustWrite(t, filepath.Join(repo, "mod", "add_test.go"),
		"package mod\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 2) != 4 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n")
	runGit("add", "-A")
	runGit("commit", "-m", "green committed tree")
	head := gitHead(t, repo)

	var log bytes.Buffer
	// Drift is non-empty (so the gate runs past the fast path) but unrelated
	// to the touched package; the committed tests are green.
	if err := verifyCommittedTreeCompiles(context.Background(), repo, head,
		[]string{"README.md"}, []string{"mod/add.go"}, &log); err != nil {
		t.Fatalf("green committed tree with drift must pass, got %v\nlog: %s", err, log.String())
	}
}

// TestVerifyCommittedTreeTests_UnbuildableTestPackageSkips: a touched
// package whose test binary won't build (a test-only import that can't be
// resolved offline, the `[build failed]` shape) is gate infrastructure,
// not a genuine test failure, so it must SKIP (non-blocking) and log
// test_gate_skipped — never block a legitimate push on gate infra.
func TestVerifyCommittedTreeTests_UnbuildableTestPackageSkips(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.21\n\nuse ./mod\n")
	if err := os.MkdirAll(filepath.Join(repo, "mod"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "mod", "go.mod"), "module example.com/mod\n\ngo 1.21\n")
	mustWrite(t, filepath.Join(repo, "mod", "use.go"),
		"package mod\n\nfunc F() int { return 1 }\n")
	mustWrite(t, filepath.Join(repo, "mod", "use_test.go"),
		"package mod\n\nimport (\n\t\"testing\"\n\n\t_ \"example.com/totally/absent/pkg\"\n)\n\nfunc TestF(t *testing.T) { _ = F() }\n")
	runGit("add", "-A")
	runGit("commit", "-m", "test imports an unavailable dependency")
	head := gitHead(t, repo)

	var log bytes.Buffer
	if err := verifyCommittedTreeCompiles(context.Background(), repo, head,
		[]string{"mod/dropped.go"}, []string{"mod/use.go"}, &log); err != nil {
		t.Fatalf("unbuildable test package must NOT block, got %v\nlog: %s", err, log.String())
	}
	if !strings.Contains(log.String(), "test_gate_skipped") {
		t.Errorf("expected test_gate_skipped log, got: %s", log.String())
	}
}

// TestLooksLikeTestFailure covers the positive-allowlist classifier that
// separates a genuine test failure (block) from a test-binary build
// failure, setup failure, dep-resolution error, or success (skip). The
// asymmetry is load-bearing (#800 condition 1): block only on a positively
// identified failure, skip everything else.
func TestLooksLikeTestFailure(t *testing.T) {
	block := []string{
		"--- FAIL: TestX (0.00s)",
		"panic: runtime error: invalid memory address",
	}
	for _, s := range block {
		if !looksLikeTestFailure(s) {
			t.Errorf("looksLikeTestFailure(%q) = false, want true", s)
		}
	}
	skip := []string{
		"FAIL\texample.com/mod [build failed]",
		"FAIL\texample.com/mod [setup failed]",
		"ok  \texample.com/mod\t0.012s",
		"?   \texample.com/mod\t[no test files]",
		"no required module provides package example.com/x; to add it: go get example.com/x",
		"",
	}
	for _, s := range skip {
		if looksLikeTestFailure(s) {
			t.Errorf("looksLikeTestFailure(%q) = true, want false", s)
		}
	}
}

// TestTouchedPackageArgs covers the module-relative package derivation
// from declared scope files (#800): only files under the module map to a
// `./<reldir>/...` pattern (root-level files map to `.`), distinct dirs
// dedupe, and files in other modules are excluded.
func TestTouchedPackageArgs(t *testing.T) {
	got := touchedPackageArgs("./backend", []string{
		"backend/internal/gitops/commit.go",
		"backend/internal/gitops/push.go", // same dir → dedup
		"backend/main.go",                 // root → "."
		"runner/cmd/x.go",                 // other module → excluded
		"docs/ARCHITECTURE.md",            // other module → excluded
	})
	want := []string{"./internal/gitops/...", "."}
	if len(got) != len(want) {
		t.Fatalf("touchedPackageArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("touchedPackageArgs = %v, want %v", got, want)
		}
	}
	if args := touchedPackageArgs("./runner", []string{"backend/x.go"}); args != nil {
		t.Errorf("no files under module should yield nil, got %v", args)
	}
}

// TestResolvePolicyBaseRef covers base-ref selection across the three
// cases: standalone, first decomposition child, and subsequent child.
// It drives the remoteBranchExists seam (#765).
func TestResolvePolicyBaseRef(t *testing.T) {
	const sharedRunID = "abcdef0123456789"
	sharedBranch := "fishhawk/run-" + shortID(sharedRunID)

	t.Run("standalone returns checkBaseRef", func(t *testing.T) {
		// No seam override needed: the empty decomposedFromRunID short-
		// circuits before remoteBranchExists is consulted.
		cfg := config{checkBaseRef: "main"}
		if got := resolvePolicyBaseRef(context.Background(), cfg, io.Discard); got != "main" {
			t.Fatalf("resolvePolicyBaseRef = %q, want %q", got, "main")
		}
	})

	t.Run("first child (shared branch absent) returns checkBaseRef", func(t *testing.T) {
		withFakeRemoteBranchExists(t, false)
		cfg := config{checkBaseRef: "main", decomposedFromRunID: sharedRunID}
		if got := resolvePolicyBaseRef(context.Background(), cfg, io.Discard); got != "main" {
			t.Fatalf("resolvePolicyBaseRef = %q, want %q", got, "main")
		}
	})

	t.Run("subsequent child (shared branch present) returns origin/<shared-branch>", func(t *testing.T) {
		withFakeRemoteBranchExists(t, true)
		cfg := config{checkBaseRef: "main", decomposedFromRunID: sharedRunID, stageID: "s1"}
		want := "origin/" + sharedBranch
		if got := resolvePolicyBaseRef(context.Background(), cfg, io.Discard); got != want {
			t.Fatalf("resolvePolicyBaseRef = %q, want %q", got, want)
		}
	})
}

// TestComputeAndEmitDiff_DecompositionChild_MeasuresIncrement is the
// end-to-end proof for #765: it crosses resolvePolicyBaseRef -> gitdiff
// -> constraint.Evaluate against a real git repo. A 2-child
// decomposition whose CUMULATIVE file count exceeds the cap passes
// because each child's increment is measured against the prior
// shared-branch tip; the pre-fix `main` base would have failed.
func TestComputeAndEmitDiff_DecompositionChild_MeasuresIncrement(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "--initial-branch=main")
	runGit("config", "user.name", "init")
	runGit("config", "user.email", "init@example.com")
	runGit("config", "commit.gpgsign", "false")
	runGit("config", "tag.gpgsign", "false")

	// Base commit on main.
	if err := os.WriteFile(filepath.Join(repo, "base.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "base")

	const maxFiles = 5

	// Child A: a commit on the shared branch carrying enough files that
	// the cumulative count (A + B) exceeds the cap.
	sharedRunID := "abcdef0123456789"
	sharedBranch := "fishhawk/run-" + shortID(sharedRunID)
	runGit("checkout", "-b", sharedBranch)
	const childAFiles = 4
	for i := 0; i < childAFiles; i++ {
		name := fmt.Sprintf("a%d.go", i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit("add", "-A")
	runGit("commit", "-m", "child A")

	// Mirror the post-push remote-tracking ref so remoteBranchExists and
	// `git diff origin/<shared-branch>` both resolve the child-A tip
	// without a network fetch.
	runGit("update-ref", "refs/remotes/origin/"+sharedBranch, "HEAD")

	// Child B increment: a small set of NEW files staged but not
	// committed, under the cap on its own.
	const childBFiles = 2
	for i := 0; i < childBFiles; i++ {
		name := fmt.Sprintf("b%d.go", i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Sanity: cumulative (A + B) exceeds the cap, each increment is under it.
	if childAFiles+childBFiles <= maxFiles {
		t.Fatalf("test misconfigured: cumulative %d must exceed maxFiles %d", childAFiles+childBFiles, maxFiles)
	}
	if childBFiles > maxFiles {
		t.Fatalf("test misconfigured: child-B increment %d must be under maxFiles %d", childBFiles, maxFiles)
	}

	cfg := config{
		workingDir:          repo,
		checkBaseRef:        "main",
		decomposedFromRunID: sharedRunID,
		stageID:             "s-childB",
	}

	// Subsequent child: remoteBranchExists reports the shared branch is
	// present, so the policy diff resolves against origin/<shared-branch>.
	withFakeRemoteBranchExists(t, true)
	d, _, err := computeAndEmitDiff(cfg, io.Discard)
	if err != nil {
		t.Fatalf("computeAndEmitDiff: %v", err)
	}
	if got := len(d.ChangedFiles); got != childBFiles {
		t.Fatalf("increment diff = %d files (%v), want %d (the child-B increment only)",
			got, d.ChangedFiles, childBFiles)
	}
	if v := constraint.Evaluate(d, constraint.Constraints{MaxFilesChanged: maxFiles}); len(v) != 0 {
		t.Fatalf("increment of %d under maxFiles %d should not violate; got %v", childBFiles, maxFiles, v)
	}

	// Pre-fix behavior: the same staged index measured against `main`
	// yields the cumulative set that exceeds the cap (the #765 bug).
	gd := &gitdiff.Runner{}
	cumulative, err := gd.Run(context.Background(), "main", repo)
	if err != nil {
		t.Fatalf("cumulative diff: %v", err)
	}
	if got := len(cumulative.ChangedFiles); got != childAFiles+childBFiles {
		t.Fatalf("cumulative diff = %d files, want %d", got, childAFiles+childBFiles)
	}
	if v := constraint.Evaluate(cumulative, constraint.Constraints{MaxFilesChanged: maxFiles}); len(v) == 0 {
		t.Fatalf("cumulative of %d over maxFiles %d must violate (pre-fix repro)", childAFiles+childBFiles, maxFiles)
	}
}

// TestComputeAndEmitDiff_CategorizesScopeDrift proves the per-path A/B
// drift categorization (#991) against a real git repo: an out-of-scope
// MODIFIED tracked file is category A (edit excluded from the commit),
// an out-of-scope CREATED file is category B (would fail the #818/#825
// gate), and the legacy `undeclared` list still names both unchanged.
// A decomposed child's B paths get excluded_from_commit instead —
// children are exempt from the created-out-of-scope gate.
func TestComputeAndEmitDiff_CategorizesScopeDrift(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	type driftEvent struct {
		Undeclared            []string            `json:"undeclared"`
		UndeclaredCategorized []driftPathEvidence `json:"undeclared_categorized"`
	}
	// setupRepo builds a repo with one declared in-scope edit, one
	// out-of-scope tracked-file edit (category A), and one out-of-scope
	// created file (category B), then returns the decoded scope_drift
	// policy_event computeAndEmitDiff emitted, plus its raw payload so
	// callers can assert key absence (omitted vs. null/empty).
	driftFor := func(t *testing.T, cfg config, logSink io.Writer) (driftEvent, []byte) {
		t.Helper()
		repo := t.TempDir()
		runGit := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = repo
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
			}
		}
		runGit("init", "--initial-branch=main")
		runGit("config", "user.name", "init")
		runGit("config", "user.email", "init@example.com")
		runGit("config", "commit.gpgsign", "false")

		for _, name := range []string{"declared.go", "tracked.go"} {
			if err := os.WriteFile(filepath.Join(repo, name), []byte("package x\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		runGit("add", "-A")
		runGit("commit", "-m", "base")

		// In-scope edit, out-of-scope tracked edit (A), out-of-scope creation (B).
		if err := os.WriteFile(filepath.Join(repo, "declared.go"), []byte("package x // edit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "tracked.go"), []byte("package x // drift\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "created.go"), []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg.workingDir = repo
		cfg.checkBaseRef = "main"
		cfg.scopeFiles = []upload.ScopeFile{{Path: "declared.go", Operation: "modify"}}
		_, events, err := computeAndEmitDiff(cfg, logSink)
		if err != nil {
			t.Fatalf("computeAndEmitDiff: %v", err)
		}
		for _, e := range events {
			if e.Kind != "policy_event" {
				continue
			}
			var w struct {
				Check   string `json:"check"`
				Outcome string `json:"outcome"`
				driftEvent
			}
			if err := json.Unmarshal(e.Payload, &w); err != nil {
				t.Fatalf("decode policy_event: %v", err)
			}
			if w.Check == "scope_drift" && w.Outcome == "excluded" {
				return w.driftEvent, e.Payload
			}
		}
		t.Fatal("no scope_drift policy_event emitted")
		return driftEvent{}, nil
	}

	assertCategorized := func(t *testing.T, got []driftPathEvidence, wantCreatedDisposition string) {
		t.Helper()
		want := map[string]driftPathEvidence{
			"tracked.go": {Path: "tracked.go", Category: "A", Disposition: "excluded_from_commit"},
			"created.go": {Path: "created.go", Category: "B", Disposition: wantCreatedDisposition},
		}
		if len(got) != len(want) {
			t.Fatalf("undeclared_categorized = %+v, want entries for %v", got, want)
		}
		for _, dp := range got {
			if w, ok := want[dp.Path]; !ok || dp != w {
				t.Errorf("undeclared_categorized entry = %+v, want %+v", dp, w)
			}
		}
	}

	t.Run("standalone", func(t *testing.T) {
		ev, _ := driftFor(t, config{stageID: "s-standalone"}, io.Discard)
		// The legacy list is untouched by categorization: both drift
		// paths, exactly as today's consumers read them.
		if len(ev.Undeclared) != 2 {
			t.Fatalf("undeclared = %v, want both drift paths", ev.Undeclared)
		}
		assertCategorized(t, ev.UndeclaredCategorized, "would_fail_loud")
	})

	t.Run("decomposed child", func(t *testing.T) {
		// First child of a fan-out (shared branch not yet pushed) so the
		// policy base stays main; only the disposition routing differs.
		withFakeRemoteBranchExists(t, false)
		ev, _ := driftFor(t, config{stageID: "s-child", decomposedFromRunID: "abcdef0123456789"}, io.Discard)
		assertCategorized(t, ev.UndeclaredCategorized, "excluded_from_commit")
	})

	t.Run("categorize failure degrades to uncategorized", func(t *testing.T) {
		// gitops.UntrackedPaths only fails when git ls-files itself
		// fails, which this temp-repo setup cannot force while
		// StageScoped succeeds in the same call — swap the seam to
		// drive the best-effort degradation branch: a one-line
		// scope_drift_categorize_failed log entry and today's
		// uncategorized payload, never a stage failure.
		orig := untrackedPaths
		untrackedPaths = func(context.Context, string, []string) ([]string, error) {
			return nil, errors.New("ls-files exploded")
		}
		t.Cleanup(func() { untrackedPaths = orig })

		var log bytes.Buffer
		ev, raw := driftFor(t, config{stageID: "s-degraded"}, &log)

		if len(ev.Undeclared) != 2 {
			t.Fatalf("undeclared = %v, want both drift paths", ev.Undeclared)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("decode payload keys: %v", err)
		}
		if _, ok := keys["undeclared_categorized"]; ok {
			t.Errorf("payload = %s, want undeclared_categorized omitted after categorize failure", raw)
		}
		logged := log.String()
		if !strings.Contains(logged, `"event":"scope_drift_categorize_failed"`) ||
			!strings.Contains(logged, `"stage_id":"s-degraded"`) ||
			!strings.Contains(logged, "ls-files exploded") {
			t.Errorf("log = %q, want scope_drift_categorize_failed with stage_id and error detail", logged)
		}
	})
}

// TestDecompositionFanout_TrackingRefMaterialized_RoutesSubsequent crosses
// the real gitops push -> tracking-ref -> production remoteBranchExists ->
// routing seam end-to-end (#770). The bug lives in that seam: a decomposed
// child pushes the shared branch with a URL push (git push <url> HEAD:<branch>),
// which never updates refs/remotes/origin/<branch>, so the production
// remoteBranchExists read sees the branch as absent and mis-routes the next
// child to the first-child `checkout -b` path (which then fails because the
// local branch already exists). Per-layer unit tests on a swapped fake pusher
// could not catch this — it only surfaces when the REAL Pusher's URL push
// feeds the REAL remoteBranchExists. This is the same git-state-staleness
// family as #767 (the stale --force-with-lease, fixed by the same maintained
// ref). The test deliberately does NOT swap newPusher or remoteBranchExists.
func TestDecompositionFanout_TrackingRefMaterialized_RoutesSubsequent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "src")
	bare := filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "--initial-branch=main")
	runGit("config", "user.name", "init")
	runGit("config", "user.email", "init@example.com")
	if err := os.WriteFile(filepath.Join(repo, "base.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-m", "base")
	runGit("init", "--bare", bare)
	runGit("remote", "add", "origin", bare)

	const parentRunID = "aaaaaaaabbbbcccc"
	sharedBranch := "fishhawk/run-" + shortID(parentRunID)
	ctx := context.Background()

	// Before any child pushes, the production routing read must report the
	// shared branch as absent (first-child path).
	if remoteBranchExists(ctx, repo, sharedBranch) {
		t.Fatal("remoteBranchExists = true before any push; want false")
	}

	// Child A: first child of the fan-out. Commit + push the shared branch
	// via the REAL gitops.Pusher (a URL push), with UpdateTrackingRef:true as
	// the runner sets for decomposed children. NOT a fake pusher — the bug is
	// in the real URL-push-vs-tracking-ref seam.
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &gitops.Pusher{}
	if _, err := p.CommitAndPush(ctx, gitops.CommitAndPushArgs{
		RepoDir:           repo,
		Branch:            sharedBranch,
		CommitMessage:     "child A",
		RemoteURL:         bare,
		ForceWithLease:    true,
		UpdateTrackingRef: true,
	}); err != nil {
		t.Fatalf("child A CommitAndPush: %v", err)
	}

	// The seam AND the routing decision in one read: the upload phase computes
	// child B's isSubsequent as exactly remoteBranchExists(ctx, repo, sharedBranch)
	// (main.go), so this single call is both the #770 regression guard and the
	// routing assertion. Even though child A pushed by URL, the production
	// remoteBranchExists now observes the shared branch as present because
	// CommitAndPush materialized refs/remotes/origin/<branch> — so child B
	// routes to RebaseFromRemote, not the first-child `checkout -b` that would
	// fail on the existing local branch. (A second identical call would add no
	// signal — git state is unchanged — so it was removed per the review.)
	if isSubsequent := remoteBranchExists(ctx, repo, sharedBranch); !isSubsequent {
		t.Fatal("child B routing: remoteBranchExists = false after child A's URL push — the #770 bug (tracking ref not materialized); want true (RebaseFromRemote path)")
	}
}

// verifiedTreeRepo builds the #960 integration fixture: a real repo with a
// local bare `origin` holding main, a `url.insteadOf` rewrite so the
// production push path's hardcoded https://github.com/<owner>/<repo> URL
// resolves to the bare repo (no network), and a dirty in-scope edit standing
// in for the agent's work. Returns the repo path, the bare path, and the
// run-branch name the production routing will derive.
func verifiedTreeRepo(t *testing.T) (repo, bare, branch string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo = filepath.Join(dir, "src")
	bare = filepath.Join(dir, "origin.git")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "--initial-branch=main")
	runGit("config", "user.name", "init")
	runGit("config", "user.email", "init@example.com")
	runGit("config", "commit.gpgsign", "false")
	mustWrite(t, filepath.Join(repo, "a.txt"), "base\n")
	runGit("add", "-A")
	runGit("commit", "-m", "base")
	runGit("init", "--bare", bare)
	runGit("remote", "add", "origin", bare)
	runGit("push", "origin", "main")
	// Production builds the push/fetch URL as https://github.com/<slug>;
	// rewrite it to the local bare repo so FreshFetchBase + push stay local.
	runGit("config", "url."+bare+".insteadOf", "https://github.com/test-owner/test-repo")

	// The agent's in-scope edit.
	mustWrite(t, filepath.Join(repo, "a.txt"), "agent change\n")

	branch = fmt.Sprintf("fishhawk/run-%s/stage-%s", shortID(verifiedTreeRunID), shortID(verifiedTreeStageID))
	return repo, bare, branch
}

const (
	verifiedTreeRunID   = "11112222333344445555666677778888"
	verifiedTreeStageID = "99990000aaaabbbbccccddddeeeeffff"
)

// verifiedTreeCfg is the config openPRAndShipArtifact sees for the #960
// integration tests: a standalone implement push (default branch routing →
// FreshFetchBase set) against the rewritten github URL.
func verifiedTreeCfg(repo, verifyCmd string) config {
	return config{
		runID:      verifiedTreeRunID,
		stageID:    verifiedTreeStageID,
		workingDir: repo,
		githubRepo: "test-owner/test-repo",
		baseBranch: "main",
		backendURL: "https://api.fishhawk.test",
		verifyCmd:  verifyCmd,
		scopeFiles: []upload.ScopeFile{{Path: "a.txt", Operation: "modify"}},
	}
}

// unresolvableTreeVerifyCmd returns a verify command that PASSES (exit 0) but
// leaves the throwaway commit's tree unresolvable in the main repo: running in
// the gate's isolated worktree (shared object store), it soft-resets the main
// repo's HEAD off the throwaway commit FIRST (so the gate's own post-verify
// reset still resolves HEAD~1 — the fixture needs two base commits for that
// second step back) and then deletes the throwaway commit's loose object, so
// the gate's fail-closed gitRevParseTreeOf(headSHA) capture errors after a
// real pass — the outcome=="passed" && treeErr != nil branch (#960 approval
// condition).
func unresolvableTreeVerifyCmd(repo string) string {
	return fmt.Sprintf(
		`sha=$(git rev-parse HEAD) && git -C %q reset --soft HEAD~1 && rm -f %q/.git/objects/$(printf %%s "$sha" | cut -c1-2)/$(printf %%s "$sha" | cut -c3-)`,
		repo, repo)
}

// addSecondBaseCommit commits one extra tracked file on top of the fixture's
// base WITHOUT staging the dirty in-scope edit, so a repo whose HEAD was
// already soft-reset once by unresolvableTreeVerifyCmd still has a parent for
// the gate's own reset --soft HEAD~1.
func addSecondBaseCommit(t *testing.T, repo string) {
	t.Helper()
	mustWrite(t, filepath.Join(repo, "extra.txt"), "second base commit\n")
	for _, args := range [][]string{{"add", "extra.txt"}, {"commit", "-m", "second base commit"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// moveBareMain advances the bare remote's main by one commit made in a side
// clone — the "origin/<base> moved between gate and push" shape that makes
// FreshFetchBase produce a pushed tree the gates never saw.
func moveBareMain(t *testing.T, bare string) {
	t.Helper()
	side := filepath.Join(t.TempDir(), "side")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(filepath.Dir(side), "clone", "-b", "main", bare, side)
	run(side, "config", "user.name", "other")
	run(side, "config", "user.email", "other@example.com")
	run(side, "config", "commit.gpgsign", "false")
	mustWrite(t, filepath.Join(side, "b.txt"), "moved base\n")
	run(side, "add", "-A")
	run(side, "commit", "-m", "base moved by another writer")
	run(side, "push", "origin", "main")
}

// withFakePROpenerOnly swaps ONLY the PR-opener seam, keeping the REAL
// gitops.Pusher — the #960 integration tests cross the closure → gitops push
// boundary for real, so unlike withFakeGitOps the pusher must not be faked.
func withFakePROpenerOnly(t *testing.T) *fakePROpener {
	t.Helper()
	fpr := &fakePROpener{}
	orig := newPROpener
	newPROpener = func(token string) prOpener {
		fpr.gotToken = token
		return fpr
	}
	t.Cleanup(func() { newPROpener = orig })
	return fpr
}

// TestRunVerifyGateCommitted_ReturnsVerifiedTreeSHA: on a pass the single-shot
// gate returns the throwaway commit's tree object hash (#960) and stamps the
// verify_run event with tree_sha. The returned hash must equal the tree of an
// equivalent real commit of the same snapshot (tree hashes are metadata-
// independent), which is exactly the equivalence the pre-push invariant
// relies on.
func TestRunVerifyGateCommitted_ReturnsVerifiedTreeSHA(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "true")
	evs, tree, err := runVerifyGateCommitted(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("runVerifyGateCommitted: %v", err)
	}
	if tree == "" {
		t.Fatal("passing gate must return the verified tree hash")
	}
	if len(evs) != 1 {
		t.Fatalf("event count = %d, want 1 (pass path)", len(evs))
	}
	if !strings.Contains(string(evs[0].Payload), fmt.Sprintf(`"tree_sha":%q`, tree)) {
		t.Errorf("verify_run payload must carry tree_sha %q:\n%s", tree, evs[0].Payload)
	}
	// Commit the same snapshot for real; its tree must equal the gate's.
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-m", "real commit, different metadata"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Fatalf("git %v: %v\n%s", args, cerr, out)
		}
	}
	wantTree, terr := gitRevParseTreeOf(context.Background(), repo, "HEAD")
	if terr != nil {
		t.Fatal(terr)
	}
	if tree != wantTree {
		t.Errorf("verified tree = %q, want %q (tree of an identical-snapshot commit)", tree, wantTree)
	}
}

// TestRunVerifyGateCommitted_FailReturnsEmptyTree: a failing verify returns
// the ErrCommittedTestsFailed demotion and NO verified tree — the
// empty-string/no-enforcement convention is reserved for no-pass paths.
func TestRunVerifyGateCommitted_FailReturnsEmptyTree(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "false")
	_, tree, err := runVerifyGateCommitted(context.Background(), cfg, io.Discard)
	if !errors.Is(err, gitops.ErrCommittedTestsFailed) {
		t.Fatalf("err = %v, want ErrCommittedTestsFailed", err)
	}
	if tree != "" {
		t.Errorf("failing gate must return an empty verified tree, got %q", tree)
	}
}

// TestRunVerifyFixLoop_ReturnsVerifiedTreeSHA: the fix loop's passing
// iteration returns the verified tree hash; exhaustion returns empty.
func TestRunVerifyFixLoop_ReturnsVerifiedTreeSHA(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "true")
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), cfg, &fakeInvoker{canned: agent.Result{OK: true}}, agent.Invocation{}, &res, io.Discard)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v", err)
	}
	if reinvoked {
		t.Error("first-iteration pass must not reinvoke")
	}
	if tree == "" {
		t.Error("passing fix loop must return the verified tree hash")
	}
}

// TestRunVerifyFixLoop_ExhaustionReturnsEmptyTree: when the budget runs out
// without a pass, no verified tree is returned (no enforcement — the stage is
// demoted to category-A anyway).
func TestRunVerifyFixLoop_ExhaustionReturnsEmptyTree(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "false")
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	_, tree, err := runVerifyFixLoop(context.Background(), cfg, &fakeInvoker{canned: agent.Result{OK: true}}, agent.Invocation{}, &res, io.Discard)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v", err)
	}
	if tree != "" {
		t.Errorf("exhausted fix loop must return an empty verified tree, got %q", tree)
	}
	if res.OK || res.FailureCategory != "A" {
		t.Errorf("exhaustion must demote to category-A; OK=%t category=%q", res.OK, res.FailureCategory)
	}
}

// TestRunVerifyCommittedTree_OutcomeStrings pins the outcome-string contract
// (#960): "passed" on exit 0, "failed" on non-zero, and "skipped" on a gate
// infra failure (forced here via an unresolvable head SHA so worktree_add
// fails) — the lossy ok bool used to return true for infra paths,
// indistinguishable from a real pass.
func TestRunVerifyCommittedTree_OutcomeStrings(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("add", "a.txt")
	runGit("commit", "-m", "committed tree")
	head, err := gitRevParseHEAD(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}

	if ev, _, outcome := runVerifyCommittedTree(context.Background(), "true", repo, head, time.Minute); outcome != "passed" {
		t.Errorf("exit-0 outcome = %q, want passed (%s)", outcome, ev.Payload)
	}
	if _, _, outcome := runVerifyCommittedTree(context.Background(), "false", repo, head, time.Minute); outcome != "failed" {
		t.Errorf("non-zero outcome = %q, want failed", outcome)
	}
	bogus := strings.Repeat("deadbeef", 5)
	if ev, _, outcome := runVerifyCommittedTree(context.Background(), "true", repo, bogus, time.Minute); outcome != "skipped" {
		t.Errorf("worktree_add-failure outcome = %q, want skipped (%s)", outcome, ev.Payload)
	}
}

// TestOpenPRAndShipArtifact_VerifiedTreeMismatch_FailedReverifyBlocksPush is
// the #960 cross-layer seam test (the shape that broke on run 07bce059): the
// committed-tree gate verifies tree T, origin/<base> then moves, so the real
// commit CommitAndPush builds (fresh-fetched base + same scope edit) carries
// a DIFFERENT tree. The pre-push hook must detect the mismatch, run the
// strict re-verify, and — when it fails — return ErrPushedTreeNotVerified
// with NO ref created on the bare remote.
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_FailedReverifyBlocksPush(t *testing.T) {
	repo, bare, branch := verifiedTreeRepo(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Gate passes against the pre-move base; capture the verified tree.
	_, verifiedTree, gerr := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), io.Discard)
	if gerr != nil || verifiedTree == "" {
		t.Fatalf("gate: tree=%q err=%v", verifiedTree, gerr)
	}

	moveBareMain(t, bare)

	// The re-verify command fails → the push must be blocked.
	var logSink strings.Builder
	err = openPRAndShipArtifact(context.Background(), verifiedTreeCfg(repo, "false"), &logSink, fu, issued, "", false, false, nil, false, verifiedTree)
	if !errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Fatalf("err = %v, want ErrPushedTreeNotVerified", err)
	}
	if !strings.Contains(logSink.String(), `"event":"verified_tree_mismatch"`) {
		t.Errorf("expected verified_tree_mismatch event:\n%s", logSink.String())
	}
	// The verify_run record is emitted unconditionally — failed re-verifies
	// leave the record too (#969).
	if !strings.Contains(logSink.String(), `"event":"verify_run"`) ||
		!strings.Contains(logSink.String(), `"outcome":"failed"`) {
		t.Errorf("expected a verify_run log event with outcome failed:\n%s", logSink.String())
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not run after a blocked push")
	}
	// No ref may have reached the bare remote — origin untouched.
	if out, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+branch).Output(); rerr == nil {
		t.Errorf("run branch %s reached the bare remote despite the blocked push (tip %s)", branch, strings.TrimSpace(string(out)))
	}
}

// TestOpenPRAndShipArtifact_VerifiedTreeMismatch_PassingReverifyPushes: same
// moved-base mismatch, but the strict re-verify passes — the pushed SHA is
// now itself gate-verified, so the push proceeds, pushed_tree_reverified
// carries BOTH trees (original gate tree + re-verified tree), the re-verify's
// verify_run record is emitted, and pull_request_opened stamps the RE-VERIFIED
// tree as verified_tree_sha so verified_tree_sha == tree_sha holds (#969).
func TestOpenPRAndShipArtifact_VerifiedTreeMismatch_PassingReverifyPushes(t *testing.T) {
	repo, bare, branch := verifiedTreeRepo(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, verifiedTree, gerr := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, "true"), io.Discard)
	if gerr != nil || verifiedTree == "" {
		t.Fatalf("gate: tree=%q err=%v", verifiedTree, gerr)
	}

	moveBareMain(t, bare)

	var logSink strings.Builder
	if err := openPRAndShipArtifact(context.Background(), verifiedTreeCfg(repo, "true"), &logSink, fu, issued, "", false, false, nil, false, verifiedTree); err != nil {
		t.Fatalf("openPRAndShipArtifact: %v\n%s", err, logSink.String())
	}
	logs := logSink.String()
	if !strings.Contains(logs, `"event":"verified_tree_mismatch"`) {
		t.Errorf("expected verified_tree_mismatch before the re-verify:\n%s", logs)
	}
	if fpr.gotArgs == nil {
		t.Error("OpenPR should have run after the re-verified push")
	}
	// Resolve the pushed tree independently from the bare remote; the
	// moved-base premise means it must differ from the original gate tree.
	pushedTreeOut, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "refs/heads/"+branch+"^{tree}").Output()
	if rerr != nil {
		t.Fatalf("run branch %s missing from the bare remote after a re-verified push: %v", branch, rerr)
	}
	pushedTree := strings.TrimSpace(string(pushedTreeOut))
	if pushedTree == verifiedTree {
		t.Fatalf("moved-base premise broken: pushed tree %s equals the original gate tree", pushedTree)
	}
	var prLine, reverifiedLine, verifyRunLine string
	for _, line := range strings.Split(logs, "\n") {
		switch {
		case strings.Contains(line, `"event":"pull_request_opened"`):
			prLine = line
		case strings.Contains(line, `"event":"pushed_tree_reverified"`):
			reverifiedLine = line
		case strings.Contains(line, `"event":"verify_run"`):
			verifyRunLine = line
		}
	}
	// The rebind (#969): pull_request_opened stamps the RE-VERIFIED pushed
	// tree as verified_tree_sha, so the stamped pair is equal.
	if !strings.Contains(prLine, fmt.Sprintf(`"verified_tree_sha":%q,"tree_sha":%q`, pushedTree, pushedTree)) {
		t.Errorf("pull_request_opened must stamp verified_tree_sha == tree_sha == re-verified tree %q:\n%s", pushedTree, logs)
	}
	// Forensics: pushed_tree_reverified carries BOTH trees — the original
	// gate tree and the re-verified pushed tree.
	if !strings.Contains(reverifiedLine, fmt.Sprintf(`"verified_tree_sha":%q,"tree_sha":%q`, verifiedTree, pushedTree)) {
		t.Errorf("pushed_tree_reverified must carry original gate tree %q and re-verified tree %q:\n%s", verifiedTree, pushedTree, logs)
	}
	// The re-verify's verify_run record — the gate's first verify_run shipped
	// in the (already sealed) trace bundle; this is the decisive re-verify's.
	if !strings.Contains(verifyRunLine, `"outcome":"passed"`) ||
		!strings.Contains(verifyRunLine, fmt.Sprintf(`"tree_sha":%q`, pushedTree)) {
		t.Errorf("expected a verify_run log event with outcome passed and tree_sha %q:\n%s", pushedTree, logs)
	}
}

// TestOpenPRAndShipArtifact_VerifiedTreeMatch_NoReverify pins the happy path:
// identical trees push with ZERO extra verify-command invocations, observed
// via a counter-file verify command — tree-hash equality transfers the gates'
// verdict for free.
func TestOpenPRAndShipArtifact_VerifiedTreeMatch_NoReverify(t *testing.T) {
	repo, bare, branch := verifiedTreeRepo(t)
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	counter := filepath.Join(t.TempDir(), "verify-invocations")
	countCmd := fmt.Sprintf("echo run >> %q", counter)

	_, verifiedTree, gerr := runVerifyGateCommitted(context.Background(), verifiedTreeCfg(repo, countCmd), io.Discard)
	if gerr != nil || verifiedTree == "" {
		t.Fatalf("gate: tree=%q err=%v", verifiedTree, gerr)
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Fatalf("verify invocations after gate = %d, want 1", lines)
	}

	var logSink strings.Builder
	if err := openPRAndShipArtifact(context.Background(), verifiedTreeCfg(repo, countCmd), &logSink, fu, issued, "", false, false, nil, false, verifiedTree); err != nil {
		t.Fatalf("openPRAndShipArtifact: %v\n%s", err, logSink.String())
	}
	if lines := countLines(t, counter); lines != 1 {
		t.Errorf("verify invocations after push = %d, want 1 (equal trees must not re-verify)", lines)
	}
	if !strings.Contains(logSink.String(), `"event":"verified_tree_match"`) {
		t.Errorf("expected verified_tree_match on the equal-trees path:\n%s", logSink.String())
	}
	if strings.Contains(logSink.String(), `"event":"verified_tree_mismatch"`) {
		t.Errorf("equal trees must not log a mismatch:\n%s", logSink.String())
	}
	if _, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+branch).Output(); rerr != nil {
		t.Errorf("run branch %s missing from the bare remote: %v", branch, rerr)
	}
}

// TestOpenPRAndShipArtifact_EmptyVerifiedTree_NoOp pins the disable-on-empty
// contract (#960 approval condition): verifiedTreeSHA == "" (no committed-tree
// gate ran) makes the pre-push check a pure no-op — even with a moved base
// AND a verify command that would fail, the push proceeds unchanged and no
// verified_tree_* event is emitted.
func TestOpenPRAndShipArtifact_EmptyVerifiedTree_NoOp(t *testing.T) {
	repo, bare, branch := verifiedTreeRepo(t)
	withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	moveBareMain(t, bare)

	var logSink strings.Builder
	if err := openPRAndShipArtifact(context.Background(), verifiedTreeCfg(repo, "false"), &logSink, fu, issued, "", false, false, nil, false, ""); err != nil {
		t.Fatalf("empty verifiedTreeSHA must disable the check, got: %v\n%s", err, logSink.String())
	}
	for _, ev := range []string{"verified_tree_match", "verified_tree_mismatch", "pushed_tree_reverified", "verify_run"} {
		if strings.Contains(logSink.String(), `"event":"`+ev+`"`) {
			t.Errorf("no-gate path must not emit %s:\n%s", ev, logSink.String())
		}
	}
	if _, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+branch).Output(); rerr != nil {
		t.Errorf("run branch %s missing from the bare remote on the no-op path: %v", branch, rerr)
	}
}

// TestRunVerifyGateCommitted_PassedButUnresolvableTree_FailsClosed exercises
// the single-shot gate's fail-closed branch (#960 approval condition): the
// verify command PASSES but the throwaway commit's tree can no longer be
// resolved (outcome=="passed" && treeErr != nil), which must return
// ErrPushedTreeNotVerified — never the silent empty-tree downgrade that would
// disable the pre-push invariant for a gate that DID pass.
func TestRunVerifyGateCommitted_PassedButUnresolvableTree_FailsClosed(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	addSecondBaseCommit(t, repo)
	cfg := verifiedTreeCfg(repo, unresolvableTreeVerifyCmd(repo))
	_, tree, err := runVerifyGateCommitted(context.Background(), cfg, io.Discard)
	if !errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Fatalf("err = %v, want ErrPushedTreeNotVerified", err)
	}
	if tree != "" {
		t.Errorf("fail-closed gate must return an empty verified tree, got %q", tree)
	}
}

// TestRunVerifyFixLoop_PassedButUnresolvableTree_FailsClosed is the fix-loop
// twin: a passing iteration whose verified tree cannot be resolved aborts the
// loop hard with ErrPushedTreeNotVerified (fatalErr path) instead of
// returning passed-with-no-enforcement.
func TestRunVerifyFixLoop_PassedButUnresolvableTree_FailsClosed(t *testing.T) {
	repo, _, _ := verifiedTreeRepo(t)
	addSecondBaseCommit(t, repo)
	cfg := verifiedTreeCfg(repo, unresolvableTreeVerifyCmd(repo))
	cfg.verifyMaxIterations = 1
	res := agent.Result{OK: true}
	reinvoked, tree, err := runVerifyFixLoop(context.Background(), cfg, &fakeInvoker{canned: agent.Result{OK: true}}, agent.Invocation{}, &res, io.Discard)
	if !errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Fatalf("err = %v, want ErrPushedTreeNotVerified", err)
	}
	if reinvoked {
		t.Error("the verify passed, so the fail-closed abort must not have reinvoked the agent")
	}
	if tree != "" {
		t.Errorf("fail-closed fix loop must return an empty verified tree, got %q", tree)
	}
}

// TestOpenPRAndShipArtifact_UnresolvableStagedTree_FailsClosed exercises the
// pre-push hook's terr != nil arm (#960 approval condition): a committed-tree
// gate passed (non-empty verifiedTreeSHA), but the REAL staged commit's tree
// cannot be resolved — the push must be blocked with ErrPushedTreeNotVerified,
// never attempted with an unprovable tree. CommitAndPush resolves HEAD^{tree}
// itself before invoking the hook, so the only way the hook's own rev-parse
// can fail is the commit object vanishing in between — forced here via a
// post-checkout git hook fired by the compile gate's isolated `git worktree
// add` (the single script-execution point between the two rev-parses), which
// deletes the just-committed object. The hook no-ops for main-repo checkouts
// (FreshFetchBase's checkout -b) so only the worktree-add trigger fires.
func TestOpenPRAndShipArtifact_UnresolvableStagedTree_FailsClosed(t *testing.T) {
	repo, bare, branch := verifiedTreeRepo(t)
	fpr := withFakePROpenerOnly(t)
	fu := newFakeUploader(t)
	issued, err := fu.IssueKey(context.Background(), verifiedTreeRunID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, gerr := cmd.CombinedOutput(); gerr != nil {
			t.Fatalf("git %v: %v\n%s", args, gerr, out)
		}
	}

	// The compile gate only worktree-adds when drift is non-empty AND go.work
	// exists, so commit both to the base (and push: FreshFetchBase cuts the
	// run branch from origin/main) plus an out-of-scope edit to b.txt as the
	// tracked-modified drift (untracked drift would trip the more specific
	// created-out-of-scope gate first).
	mustWrite(t, filepath.Join(repo, "go.work"), "go 1.22\n")
	mustWrite(t, filepath.Join(repo, "b.txt"), "tracked base\n")
	runGit("add", "go.work", "b.txt")
	runGit("commit", "-m", "gate fixture: go.work + tracked b.txt")
	runGit("push", "origin", "main")
	mustWrite(t, filepath.Join(repo, "b.txt"), "out-of-scope edit\n")

	repoReal, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	hook := fmt.Sprintf(`#!/bin/sh
[ "$(pwd -P)" = %q ] && exit 0
sha="$2"
rm -f %q/.git/objects/$(printf %%s "$sha" | cut -c1-2)/$(printf %%s "$sha" | cut -c3-)
exit 0
`, repoReal, repo)
	runGit("config", "core.hooksPath", ".git/hooks")
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}

	// Any non-empty verifiedTreeSHA arms the invariant; the base tree stands
	// in for a gate pass (running the real gate here would fire the hook on
	// the gate's own worktree add and fail-close there instead).
	verifiedTree, err := gitRevParseTreeOf(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	var logSink strings.Builder
	err = openPRAndShipArtifact(context.Background(), verifiedTreeCfg(repo, "true"), &logSink, fu, issued, "", false, false, nil, false, verifiedTree)
	if !errors.Is(err, gitops.ErrPushedTreeNotVerified) {
		t.Fatalf("err = %v, want ErrPushedTreeNotVerified\n%s", err, logSink.String())
	}
	// The mismatch/re-verify path wraps the same sentinel — pin THIS arm:
	// the unresolvable-tree error reports before any tree comparison.
	if !strings.Contains(err.Error(), "could not be resolved") {
		t.Errorf("err = %v, want the unresolvable-staged-tree arm (\"could not be resolved\")", err)
	}
	if strings.Contains(logSink.String(), `"event":"verified_tree_mismatch"`) {
		t.Errorf("an unresolvable tree must fail before the mismatch comparison:\n%s", logSink.String())
	}
	if fpr.gotArgs != nil {
		t.Error("OpenPR must not run after a blocked push")
	}
	// No ref may have reached the bare remote — origin untouched.
	if out, rerr := exec.Command("git", "--git-dir="+bare, "rev-parse", "--verify", "refs/heads/"+branch).Output(); rerr == nil {
		t.Errorf("run branch %s reached the bare remote despite the blocked push (tip %s)", branch, strings.TrimSpace(string(out)))
	}
}

// countLines returns the number of newline-terminated lines in path; 0 when
// the file does not exist yet.
func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// --- #961 mid-stage scope-amendment refresh ---

func amendmentCfg(scope ...upload.ScopeFile) *config {
	return &config{
		runID:      "run-abc",
		stageID:    "stage-1",
		scopeFiles: scope,
	}
}

func TestRefreshScopeAmendments_FoldsApprovedOnly(t *testing.T) {
	fake := newFakeUploader(t)
	fake.amendments = []upload.ScopeAmendment{
		{
			ID: "a1", Status: "approved",
			Paths: []upload.ScopeAmendmentPath{
				{Path: "pkg/newfile.go", Operation: "create"},
				{Path: "pkg/extra.go", Operation: "modify"},
				{Path: "pkg/in_scope.go", Operation: "modify"}, // already declared — dedupe
			},
		},
		{ID: "a2", Status: "pending", Paths: []upload.ScopeAmendmentPath{{Path: "pkg/pending.go", Operation: "modify"}}},
		{ID: "a3", Status: "denied", Paths: []upload.ScopeAmendmentPath{{Path: "pkg/denied.go", Operation: "modify"}}},
	}
	cfg := amendmentCfg(upload.ScopeFile{Path: "pkg/in_scope.go", Operation: "modify"})
	var log bytes.Buffer

	refreshScopeAmendments(context.Background(), fake, cfg, "fhm_heldbyrunner", &log)

	if fake.gotAmendmentArgs == nil {
		t.Fatal("FetchScopeAmendments never called")
	}
	if fake.gotAmendmentArgs.MCPToken != "fhm_heldbyrunner" {
		t.Errorf("bearer = %q, want the retained fhm_ token", fake.gotAmendmentArgs.MCPToken)
	}
	if fake.gotAmendmentArgs.RunID != "run-abc" {
		t.Errorf("run id = %q", fake.gotAmendmentArgs.RunID)
	}
	counts := map[string]int{}
	for _, f := range cfg.scopeFiles {
		counts[f.Path]++
	}
	for _, want := range []string{"pkg/in_scope.go", "pkg/newfile.go", "pkg/extra.go"} {
		if counts[want] != 1 {
			t.Errorf("scopeFiles[%q] = %d, want exactly 1; got %+v", want, counts[want], cfg.scopeFiles)
		}
	}
	for _, never := range []string{"pkg/pending.go", "pkg/denied.go"} {
		if counts[never] != 0 {
			t.Errorf("scopeFiles must not fold %q (undecided/denied)", never)
		}
	}
	if !strings.Contains(log.String(), "scope_amendments_folded") {
		t.Errorf("log missing scope_amendments_folded event: %s", log.String())
	}
}

func TestRefreshScopeAmendments_NoTokenOrEmptyScopeNoOps(t *testing.T) {
	fake := newFakeUploader(t)
	fake.amendments = []upload.ScopeAmendment{
		{ID: "a1", Status: "approved", Paths: []upload.ScopeAmendmentPath{{Path: "x.go", Operation: "modify"}}},
	}
	var log bytes.Buffer

	// No token → never calls the backend.
	cfg := amendmentCfg(upload.ScopeFile{Path: "pkg/in_scope.go", Operation: "modify"})
	refreshScopeAmendments(context.Background(), fake, cfg, "", &log)
	if fake.gotAmendmentArgs != nil {
		t.Error("refresh called the backend without a token")
	}
	if len(cfg.scopeFiles) != 1 {
		t.Errorf("scope changed: %+v", cfg.scopeFiles)
	}

	// Empty scope (git add -A fallback) → never narrows.
	cfg = amendmentCfg()
	refreshScopeAmendments(context.Background(), fake, cfg, "fhm_held", &log)
	if fake.gotAmendmentArgs != nil {
		t.Error("refresh called the backend on an empty scope")
	}
	if len(cfg.scopeFiles) != 0 {
		t.Errorf("empty scope was augmented: %+v", cfg.scopeFiles)
	}
}

func TestRefreshScopeAmendments_FetchErrorKeepsScope(t *testing.T) {
	fake := newFakeUploader(t)
	fake.amendmentsErr = errors.New("backend unreachable")
	cfg := amendmentCfg(upload.ScopeFile{Path: "pkg/in_scope.go", Operation: "modify"})
	var log bytes.Buffer

	refreshScopeAmendments(context.Background(), fake, cfg, "fhm_held", &log)

	if len(cfg.scopeFiles) != 1 || cfg.scopeFiles[0].Path != "pkg/in_scope.go" {
		t.Errorf("scope changed on fetch error: %+v", cfg.scopeFiles)
	}
	if !strings.Contains(log.String(), "scope_amendment_refresh_failed") {
		t.Errorf("log missing scope_amendment_refresh_failed: %s", log.String())
	}
}

// TestScopeAmendment_RunnerEndToEnd is the runner half of the #961
// cross-boundary activation test (#618 rule). It serves the CANONICAL
// GET /scope-amendments wire fixture (the same shape upload_test.go's
// canonicalScopeAmendmentsJSON and the backend's
// scopeAmendmentListResponse pin) from an httptest backend, drives the
// REAL upload client decode -> refreshScopeAmendments fold -> a real
// StageScoped in a temp git repo, and asserts the created-file gate's
// inputs: the APPROVED create is staged (absent from drift/untracked)
// while a NON-requested net-new file still surfaces as untracked drift
// (the #818/#825 fail-loud contract).
func TestScopeAmendment_RunnerEndToEnd(t *testing.T) {
	const wire = `{
  "items": [
    {
      "id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a001",
      "run_id": "run-abc",
      "stage_id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a002",
      "paths": [
        {"path": "pkg/extra.go", "operation": "modify"},
        {"path": "pkg/newfile.go", "operation": "create"}
      ],
      "reason": "the seam needs these",
      "status": "approved",
      "decision_reason": "ok",
      "decided_by": "github:operator",
      "requested_at": "2026-06-10T12:00:00Z",
      "decided_at": "2026-06-10T12:01:00Z"
    },
    {
      "id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a003",
      "run_id": "run-abc",
      "stage_id": "0b54f9f3-0c83-4f6e-9c6e-1a54a3b1a002",
      "paths": [{"path": "pkg/denied.go", "operation": "modify"}],
      "reason": "nope",
      "status": "denied",
      "decision_reason": "out of bounds",
      "decided_by": "github:operator",
      "requested_at": "2026-06-10T12:02:00Z",
      "decided_at": "2026-06-10T12:03:00Z"
    }
  ]
}`
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, wire)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Temp repo: pkg/in_scope.go and pkg/extra.go tracked at base.
	repo, runGit := compileGateRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "pkg", "in_scope.go"), "package pkg // v1\n")
	mustWrite(t, filepath.Join(repo, "pkg", "extra.go"), "package pkg // extra v1\n")
	runGit("add", "-A")
	runGit("commit", "-m", "base")

	// The "agent" edits: declared file, amendment-approved modify,
	// amendment-approved create, and one rogue net-new file no
	// amendment ever requested.
	mustWrite(t, filepath.Join(repo, "pkg", "in_scope.go"), "package pkg // v2\n")
	mustWrite(t, filepath.Join(repo, "pkg", "extra.go"), "package pkg // extra v2\n")
	mustWrite(t, filepath.Join(repo, "pkg", "newfile.go"), "package pkg // approved create\n")
	mustWrite(t, filepath.Join(repo, "pkg", "rogue.go"), "package pkg // never requested\n")

	// Real client against the httptest backend: decode + fold.
	cfg := amendmentCfg(upload.ScopeFile{Path: "pkg/in_scope.go", Operation: "modify"})
	var log bytes.Buffer
	refreshScopeAmendments(context.Background(), upload.New(srv.URL), cfg, "fhm_heldbyrunner", &log)

	if gotAuth != "Bearer fhm_heldbyrunner" {
		t.Fatalf("Authorization = %q, want the retained fhm_ bearer", gotAuth)
	}

	// The same StageScoped call the commit phase runs, fed the folded
	// scope exactly as openPRAndShipArtifact derives it.
	drift, err := (&gitops.Pusher{}).StageScoped(context.Background(), repo, scopePaths(cfg.scopeFiles))
	if err != nil {
		t.Fatalf("StageScoped: %v", err)
	}
	driftSet := map[string]bool{}
	for _, d := range drift {
		driftSet[d] = true
	}
	for _, staged := range []string{"pkg/in_scope.go", "pkg/extra.go", "pkg/newfile.go"} {
		if driftSet[staged] {
			t.Errorf("%s must be staged (folded scope), not drift; drift = %v", staged, drift)
		}
	}
	if !driftSet["pkg/rogue.go"] {
		t.Errorf("pkg/rogue.go must remain drift (never requested); drift = %v", drift)
	}

	// The created-out-of-scope gate's exact input: untracked drift.
	// The approved create is staged so it never reaches the gate; the
	// rogue file does — the fail-loud contract holds.
	created, err := gitops.UntrackedPaths(context.Background(), repo, drift)
	if err != nil {
		t.Fatalf("UntrackedPaths: %v", err)
	}
	if len(created) != 1 || created[0] != "pkg/rogue.go" {
		t.Errorf("created gate input = %v, want exactly [pkg/rogue.go]", created)
	}
	if !strings.Contains(log.String(), "scope_amendments_folded") {
		t.Errorf("log missing scope_amendments_folded: %s", log.String())
	}
}

// TestIsBinaryArtifactDrift unit-tests the #980 advisory build-artifact
// classifier: an oversized executable hits, the Go module-binary path shape
// cmd/<name>/<name> hits without needing the file on disk, and a plain
// source file misses. The classifier is log-only — these pin the heuristics
// so the scope_drift_binary_artifact WARN fires on the incident shape (a
// 21MB fishhawk-runner binary) without flagging ordinary drift.
func TestIsBinaryArtifactDrift(t *testing.T) {
	repoDir := t.TempDir()

	// (a) Executable above the size threshold → hit, size reported.
	bigExec := filepath.Join(repoDir, "bigtool")
	if err := os.WriteFile(bigExec, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(bigExec, binaryArtifactSizeThreshold+1); err != nil {
		t.Fatal(err)
	}
	if hit, size := isBinaryArtifactDrift(repoDir, "bigtool"); !hit || size != binaryArtifactSizeThreshold+1 {
		t.Errorf("oversized executable: hit=%v size=%d, want hit=true size=%d", hit, size, binaryArtifactSizeThreshold+1)
	}

	// Executable but small → miss (a shell script is not a build artifact).
	smallExec := filepath.Join(repoDir, "script.sh")
	if err := os.WriteFile(smallExec, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if hit, _ := isBinaryArtifactDrift(repoDir, "script.sh"); hit {
		t.Error("small executable should not classify as a build artifact")
	}

	// Oversized but not executable → miss (a large fixture is not a binary).
	bigData := filepath.Join(repoDir, "fixture.json")
	if err := os.WriteFile(bigData, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(bigData, binaryArtifactSizeThreshold+1); err != nil {
		t.Fatal(err)
	}
	if hit, _ := isBinaryArtifactDrift(repoDir, "fixture.json"); hit {
		t.Error("oversized non-executable should not classify as a build artifact")
	}

	// (b) cmd/<name>/<name> shape → hit even when the file is not stattable
	// (size 0): basename equals parent dir, directly under a cmd/ segment.
	if hit, size := isBinaryArtifactDrift(repoDir, "runner/cmd/fishhawk-runner/fishhawk-runner"); !hit || size != 0 {
		t.Errorf("cmd/<name>/<name> shape: hit=%v size=%d, want hit=true size=0", hit, size)
	}
	if hit, _ := isBinaryArtifactDrift(repoDir, "cmd/tool/tool"); !hit {
		t.Error("top-level cmd/tool/tool shape should classify as a build artifact")
	}

	// Plain source files miss: basename != parent dir, or no cmd/ segment.
	for _, miss := range []string{
		"runner/cmd/fishhawk-runner/main.go",
		"backend/internal/foo/foo.go",
		"pkg/tool/tool", // <name>/<name> but not under cmd/
		"README.md",
	} {
		if hit, _ := isBinaryArtifactDrift(repoDir, miss); hit {
			t.Errorf("%s should not classify as a build artifact", miss)
		}
	}
}
