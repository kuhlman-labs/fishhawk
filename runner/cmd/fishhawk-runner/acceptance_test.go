package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// --- captureAcceptanceVerdict -------------------------------------------

func TestCaptureAcceptanceVerdict_StructuredOutputPreferred(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verdict.json")
	mustWrite(t, path, `{"verdict":"failed","failure_mode":"error"}`)
	res := agent.Result{StructuredOutput: []byte(`{"verdict":"passed"}`)}

	got, err := captureAcceptanceVerdict(res, path)
	if err != nil {
		t.Fatalf("captureAcceptanceVerdict: %v", err)
	}
	if string(got) != `{"verdict":"passed"}` {
		t.Errorf("StructuredOutput must win when both transports exist, got %s", got)
	}
}

func TestCaptureAcceptanceVerdict_FileFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verdict.json")
	mustWrite(t, path, `{"verdict":"passed"}`)

	got, err := captureAcceptanceVerdict(agent.Result{}, path)
	if err != nil {
		t.Fatalf("captureAcceptanceVerdict: %v", err)
	}
	if string(got) != `{"verdict":"passed"}` {
		t.Errorf("file fallback bytes = %s", got)
	}
}

func TestCaptureAcceptanceVerdict_MissingBoth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	_, err := captureAcceptanceVerdict(agent.Result{}, path)
	if !errors.Is(err, errAcceptanceVerdictMissing) {
		t.Fatalf("err = %v, want errAcceptanceVerdictMissing", err)
	}
}

func TestCaptureAcceptanceVerdict_EmptyFileIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	mustWrite(t, path, "")
	_, err := captureAcceptanceVerdict(agent.Result{}, path)
	if !errors.Is(err, errAcceptanceVerdictMissing) {
		t.Fatalf("err = %v, want errAcceptanceVerdictMissing", err)
	}
}

// TestCaptureAcceptanceVerdict_ReadErrorNotMissing covers the
// non-os.IsNotExist read-error branch (#1535 fix-up): a fallback path
// that exists but cannot be read as a file (here a directory, EISDIR)
// must return the distinct wrapped "fallback read" error rather than
// errAcceptanceVerdictMissing, so a genuine read fault is not
// misattributed to a missing verdict.
func TestCaptureAcceptanceVerdict_ReadErrorNotMissing(t *testing.T) {
	dir := t.TempDir() // a directory: os.ReadFile fails with a non-not-exist error
	_, err := captureAcceptanceVerdict(agent.Result{}, dir)
	if err == nil {
		t.Fatal("expected a read error when the fallback path is a directory")
	}
	if errors.Is(err, errAcceptanceVerdictMissing) {
		t.Errorf("a non-not-exist read error must NOT be errAcceptanceVerdictMissing, got %v", err)
	}
	if !strings.Contains(err.Error(), "acceptance verdict fallback read") {
		t.Errorf("err = %v, want the wrapped fallback-read error", err)
	}
}

// --- validateAcceptanceVerdict -------------------------------------------

func TestValidateAcceptanceVerdict_Table(t *testing.T) {
	served := []string{"AC1", "AC2"}
	cases := []struct {
		name    string
		raw     string
		served  []string
		wantErr string // substring; "" = valid
	}{
		{
			name:   "valid passed with criteria",
			raw:    `{"verdict":"passed","criteria":[{"id":"AC1","result":"passed","observed":"ok","expected":"ok","steps_taken":"GET /","expectation_basis":"criterion text","repro_handle":"curl /"}],"target_url":"http://localhost:8080","evidence_hashes":["abc"]}`,
			served: served,
		},
		{
			name:   "valid failed with failure_mode",
			raw:    `{"verdict":"failed","failure_mode":"assertion_fail","criteria":[{"id":"AC2","result":"failed"}]}`,
			served: served,
		},
		{
			name:   "valid skipped result",
			raw:    `{"verdict":"passed","criteria":[{"id":"AC1","result":"skipped"}]}`,
			served: served,
		},
		{
			name:   "empty served set skips membership check",
			raw:    `{"verdict":"passed","criteria":[{"id":"anything","result":"passed"}]}`,
			served: nil,
		},
		{
			name:    "verdict missing",
			raw:     `{"criteria":[{"id":"AC1","result":"passed"}]}`,
			served:  served,
			wantErr: "verdict is required",
		},
		{
			name:    "bad verdict value",
			raw:     `{"verdict":"maybe"}`,
			served:  served,
			wantErr: "verdict must be passed or failed",
		},
		{
			name:    "failed without failure_mode",
			raw:     `{"verdict":"failed"}`,
			served:  served,
			wantErr: "failure_mode is required",
		},
		{
			name:    "bad failure_mode value",
			raw:     `{"verdict":"failed","failure_mode":"panic"}`,
			served:  served,
			wantErr: "failure_mode must be error or assertion_fail",
		},
		{
			name:    "failure_mode forbidden on passed",
			raw:     `{"verdict":"passed","failure_mode":"error"}`,
			served:  served,
			wantErr: "failure_mode must be omitted",
		},
		{
			name:    "criterion id missing",
			raw:     `{"verdict":"passed","criteria":[{"result":"passed"}]}`,
			served:  served,
			wantErr: "criteria[0].id is required",
		},
		{
			name:    "bad result value",
			raw:     `{"verdict":"passed","criteria":[{"id":"AC1","result":"meh"}]}`,
			served:  served,
			wantErr: "criteria[0].result must be passed/failed/skipped",
		},
		{
			name:    "criterion id not in served set",
			raw:     `{"verdict":"passed","criteria":[{"id":"AC9","result":"passed"}]}`,
			served:  served,
			wantErr: "not in the served acceptance_criteria_ids set",
		},
		{
			// a1 (#1567): a declared top-level notes overflow validates.
			name:   "top-level notes validates",
			raw:    `{"verdict":"passed","criteria":[{"id":"AC1","result":"passed"}],"notes":"instance came up slowly but passed"}`,
			served: served,
		},
		{
			// a2 (#1567): an UNdeclared top-level field still fails closed
			// (the fail-closed direction is preserved — only notes is tolerated).
			name:    "undeclared top-level field still fails closed",
			raw:     `{"verdict":"passed","criteria":[{"id":"AC1","result":"passed"}],"summary":"extra prose"}`,
			served:  served,
			wantErr: "could not be decoded",
		},
		{
			name:    "unknown field rejected (backend DisallowUnknownFields mirror)",
			raw:     `{"verdict":"passed","bogus":true}`,
			served:  served,
			wantErr: "could not be decoded",
		},
		{
			name:    "trailing object rejected",
			raw:     `{"verdict":"passed"}{"verdict":"failed"}`,
			served:  served,
			wantErr: "single JSON object",
		},
		{
			name:    "non-http target_url",
			raw:     `{"verdict":"passed","target_url":"ftp://x"}`,
			served:  served,
			wantErr: "target_url must be an http(s) URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAcceptanceVerdict([]byte(tc.raw), tc.served)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestAcceptanceVerdictSchema_LockstepWithValidator guards the
// three-way shape lockstep (JSON schema ↔ runner validator ↔ backend
// acceptanceBody): the schema must parse, its enums/required must match
// the rules validateAcceptanceVerdict enforces, and a maximal verdict
// exercising EVERY schema property must be validator-accepted — the
// backend validator mirrors the same rules
// (backend/internal/server/acceptance.go), so a drift in any copy
// surfaces here or in the backend's acceptance round-trip tests.
func TestAcceptanceVerdictSchema_LockstepWithValidator(t *testing.T) {
	var schema struct {
		Type                 string                     `json:"type"`
		AdditionalProperties bool                       `json:"additionalProperties"`
		Required             []string                   `json:"required"`
		Properties           map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(acceptanceVerdictJSONSchema), &schema); err != nil {
		t.Fatalf("acceptanceVerdictJSONSchema does not parse: %v", err)
	}
	if schema.AdditionalProperties {
		t.Error("schema must set additionalProperties:false (backend uses DisallowUnknownFields)")
	}
	if len(schema.Required) != 1 || schema.Required[0] != "verdict" {
		t.Errorf("schema required = %v, want [verdict] (the backend's only required field)", schema.Required)
	}
	wantProps := []string{"verdict", "failure_mode", "criteria", "target_url", "evidence_hashes", "notes"}
	if len(schema.Properties) != len(wantProps) {
		t.Errorf("schema has %d properties, want %d (%v)", len(schema.Properties), len(wantProps), wantProps)
	}
	for _, p := range wantProps {
		if _, ok := schema.Properties[p]; !ok {
			t.Errorf("schema missing property %q", p)
		}
	}
	var verdictProp struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Properties["verdict"], &verdictProp); err != nil ||
		len(verdictProp.Enum) != 2 || verdictProp.Enum[0] != "passed" || verdictProp.Enum[1] != "failed" {
		t.Errorf("verdict enum = %v (err %v), want [passed failed]", verdictProp.Enum, err)
	}
	var fmProp struct {
		Enum []string `json:"enum"`
	}
	if err := json.Unmarshal(schema.Properties["failure_mode"], &fmProp); err != nil ||
		len(fmProp.Enum) != 2 || fmProp.Enum[0] != "error" || fmProp.Enum[1] != "assertion_fail" {
		t.Errorf("failure_mode enum = %v (err %v), want [error assertion_fail]", fmProp.Enum, err)
	}
	var critProp struct {
		Items struct {
			AdditionalProperties bool                       `json:"additionalProperties"`
			Required             []string                   `json:"required"`
			Properties           map[string]json.RawMessage `json:"properties"`
		} `json:"items"`
	}
	if err := json.Unmarshal(schema.Properties["criteria"], &critProp); err != nil {
		t.Fatalf("criteria property does not parse: %v", err)
	}
	wantCritProps := []string{"id", "result", "observed", "expected", "steps_taken", "expectation_basis", "repro_handle"}
	if len(critProp.Items.Properties) != len(wantCritProps) {
		t.Errorf("criteria item has %d properties, want %d (%v)",
			len(critProp.Items.Properties), len(wantCritProps), wantCritProps)
	}
	for _, p := range wantCritProps {
		if _, ok := critProp.Items.Properties[p]; !ok {
			t.Errorf("criteria item schema missing property %q", p)
		}
	}
	if len(critProp.Items.Required) != 2 || critProp.Items.Required[0] != "id" || critProp.Items.Required[1] != "result" {
		t.Errorf("criteria item required = %v, want [id result]", critProp.Items.Required)
	}

	// A maximal verdict exercising every schema property (top-level and
	// per-criterion) must pass the runner validator.
	maximal := `{
		"verdict": "failed",
		"failure_mode": "assertion_fail",
		"criteria": [{
			"id": "AC1",
			"result": "failed",
			"observed": "500 on POST /orders",
			"expected": "201 with an order id",
			"steps_taken": "POST /orders with a valid cart",
			"expectation_basis": "criterion AC1 statement",
			"repro_handle": "curl -X POST http://target/orders -d @cart.json"
		}],
		"target_url": "http://target.example.com",
		"evidence_hashes": ["deadbeef"],
		"notes": "overall the instance behaved but AC1 regressed"
	}`
	if err := validateAcceptanceVerdict([]byte(maximal), []string{"AC1"}); err != nil {
		t.Errorf("maximal schema-shaped verdict rejected by validator: %v", err)
	}
}

// --- redaction + evidence event -------------------------------------------

func TestRedactAcceptanceVerdict_RedactsCredential(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	raw := []byte(`{"verdict":"passed","criteria":[{"id":"AC1","result":"passed","observed":"token ` + secret + ` echoed"}]}`)
	red, hits := redactAcceptanceVerdict(raw)
	if strings.Contains(string(red), secret) {
		t.Error("credential survived redaction")
	}
	if len(hits) == 0 {
		t.Error("expected at least one redaction hit")
	}
}

func TestComposeAcceptanceEvidence_Shape(t *testing.T) {
	ev := composeAcceptanceEvidence([]byte(`{"verdict":"passed"}`))
	if ev.Kind != "acceptance_evidence" {
		t.Errorf("kind = %q", ev.Kind)
	}
	var payload struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil || payload.Verdict != "passed" {
		t.Errorf("payload = %s (err %v)", ev.Payload, err)
	}
	if ev.Timestamp.IsZero() {
		t.Error("timestamp must be set")
	}
}

// --- run()-level per-failure-mode tests ------------------------------------

// acceptanceStageSetup wires the common run()-level acceptance harness:
// a real git repo as --working-dir (so worktree provisioning and the
// checkout-untouched assertions work), a fakeUploader serving an
// acceptance prompt with served criteria ids, and an overridden verdict
// fallback path so tests never touch the real shared /tmp file.
func acceptanceStageSetup(t *testing.T) (repo string, fu *fakeUploader, runArgs []string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo, runGit := compileGateRepo(t)
	mustWrite(t, filepath.Join(repo, "README.md"), "hello\n")
	runGit("add", "-A")
	runGit("commit", "-m", "initial")

	fu = newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:               "22222222-3333-4444-5555-666666666666",
		StageType:             "acceptance",
		Prompt:                "Validate the running instance against the criteria.",
		PromptHash:            "deadbeef",
		AcceptanceCriteriaIDs: []string{"AC1", "AC2"},
	}
	withFakeUploader(t, fu)

	// Point the verdict file fallback at a per-test temp path so a stale
	// real /tmp/fishhawk-acceptance.json can never bleed into a test.
	orig := acceptanceVerdictPath
	acceptanceVerdictPath = filepath.Join(t.TempDir(), "fishhawk-acceptance.json")
	t.Cleanup(func() { acceptanceVerdictPath = orig })

	runArgs = []string{
		"--run-id", "11111111-2222-3333-4444-555555555555",
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change",
		"--stage", "acceptance",
		"--stage-id", "22222222-3333-4444-5555-666666666666",
		"--fetch-prompt",
		"--working-dir", repo,
		"--upload-trace",
	}
	return repo, fu, runArgs
}

// TestRun_AcceptanceStage_ProxyStartFailure_FailsClosed: a malformed
// spec-declared egress host makes egressproxy.Start error; the stage
// fails category-C with NO agent invocation — the acceptance agent
// never runs uncontained.
func TestRun_AcceptanceStage_ProxyStartFailure_FailsClosed(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	fu.promptResp.EgressTargetHosts = []string{"bad/host"}
	invoker := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, invoker)

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if invoker.callIdx != 0 {
		t.Errorf("agent invoked %d times, want 0 (never run uncontained)", invoker.callIdx)
	}
	if !strings.Contains(stderr.String(), `"reason":"acceptance_egress_proxy"`) ||
		!strings.Contains(stderr.String(), `"category":"C"`) {
		t.Errorf("missing category-C acceptance_egress_proxy failure: %s", stderr.String())
	}
	if fu.gotAcceptanceArgs != nil {
		t.Error("ShipAcceptance must not be called on a proxy start failure")
	}
}

// TestRun_AcceptanceStage_VerdictMissing_CategoryB: the agent settled OK
// but produced neither StructuredOutput nor the fallback file — the
// stage demotes to category-B acceptance_verdict_missing and nothing
// ships.
func TestRun_AcceptanceStage_VerdictMissing_CategoryB(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, `"event":"acceptance_verdict_missing"`) {
		t.Errorf("missing acceptance_verdict_missing event: %s", out)
	}
	if !strings.Contains(out, `"category":"B"`) ||
		!strings.Contains(out, "acceptance_verdict_missing:") {
		t.Errorf("completion must be category-B with the acceptance_verdict_missing reason: %s", out)
	}
	if fu.gotAcceptanceArgs != nil {
		t.Error("ShipAcceptance must not be called for a missing verdict")
	}
}

// TestRun_AcceptanceStage_VerdictInvalid_CategoryB: a structurally
// invalid verdict (failed without failure_mode) demotes to category-B
// acceptance_verdict_invalid; the invalid bytes never ship. The full
// invalid-shape matrix is TestValidateAcceptanceVerdict_Table; this
// locks the run()-level demotion wiring.
func TestRun_AcceptanceStage_VerdictInvalid_CategoryB(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{
		OK:               true,
		StructuredOutput: []byte(`{"verdict":"failed"}`),
	}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, `"event":"acceptance_verdict_invalid"`) {
		t.Errorf("missing acceptance_verdict_invalid event: %s", out)
	}
	if !strings.Contains(out, `"category":"B"`) ||
		!strings.Contains(out, "acceptance_verdict_invalid:") {
		t.Errorf("completion must be category-B with the acceptance_verdict_invalid reason: %s", out)
	}
	if fu.gotAcceptanceArgs != nil {
		t.Error("ShipAcceptance must not be called for an invalid verdict")
	}
}

// TestRun_AcceptanceStage_UnknownCriterionID_CategoryB: a criterion id
// outside the served acceptance_criteria_ids set fails closed at the
// runner (join-key validation), category-B.
func TestRun_AcceptanceStage_UnknownCriterionID_CategoryB(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{
		OK:               true,
		StructuredOutput: []byte(`{"verdict":"passed","criteria":[{"id":"AC9","result":"passed"}]}`),
	}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not in the served acceptance_criteria_ids set") {
		t.Errorf("missing served-set membership failure: %s", stderr.String())
	}
	if fu.gotAcceptanceArgs != nil {
		t.Error("ShipAcceptance must not be called for an unknown criterion id")
	}
}

// TestRun_AcceptanceStage_FailedVerdict_ShipsAndStaysOK is the
// failed-verdict-is-not-a-runner-failure done-means test: a VALID
// verdict of failed keeps res.OK true (the validation completed —
// routing is E31.8's scope), ships the artifact, and exits 0.
func TestRun_AcceptanceStage_FailedVerdict_ShipsAndStaysOK(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{
		OK: true,
		StructuredOutput: []byte(
			`{"verdict":"failed","failure_mode":"assertion_fail","criteria":[{"id":"AC1","result":"failed","observed":"500","expected":"201"}]}`),
	}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (a failed verdict is NOT a runner failure):\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("ShipAcceptance not called for a valid failed verdict")
	}
	var shipped acceptanceVerdict
	if err := json.Unmarshal(fu.gotAcceptanceArgs.Body, &shipped); err != nil {
		t.Fatalf("shipped body does not decode: %v", err)
	}
	if shipped.Verdict != "failed" || shipped.FailureMode != "assertion_fail" {
		t.Errorf("shipped verdict/failure_mode = %q/%q", shipped.Verdict, shipped.FailureMode)
	}
	if !strings.Contains(stderr.String(), `"outcome":"ok"`) {
		t.Errorf("runner_completed must report ok: %s", stderr.String())
	}
}

// TestRun_AcceptanceStage_ShipAcceptanceInvalid_CategoryB: the backend
// rejecting the verdict shape (400 acceptance_invalid →
// ErrAcceptanceInvalid) is category-B — the agent's output is bad.
func TestRun_AcceptanceStage_ShipAcceptanceInvalid_CategoryB(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	fu.acceptanceErr = fmt.Errorf("%w: backend said no", upload.ErrAcceptanceInvalid)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{
		OK:               true,
		StructuredOutput: []byte(`{"verdict":"passed","criteria":[{"id":"AC1","result":"passed"}]}`),
	}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, `"reason":"acceptance_upload"`) {
		t.Errorf("missing acceptance_upload failure event: %s", out)
	}
	if !strings.Contains(out, `"category":"B"`) {
		t.Errorf("ErrAcceptanceInvalid must classify category-B: %s", out)
	}
}

// TestRun_AcceptanceStage_ShipInfraError_CategoryC: any other ship
// failure (network, 5xx-exhausted) is category-C infra.
func TestRun_AcceptanceStage_ShipInfraError_CategoryC(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	fu.acceptanceErr = errors.New("upload: ship acceptance exhausted retries: 503")
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{
		OK:               true,
		StructuredOutput: []byte(`{"verdict":"passed"}`),
	}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure:\n%s", got, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, `"reason":"acceptance_upload"`) ||
		!strings.Contains(out, `"category":"C"`) {
		t.Errorf("infra ship failure must classify category-C: %s", out)
	}
}

// TestRun_AcceptanceStage_RefusedPassthrough_LoggedAndAbsent: an
// operator (mis)declaring FISHHAWK_ACCEPTANCE_ENV_GITHUB_TOKEN is
// REFUSED by acceptenv (the deny set outranks the passthrough), the
// refusal is logged (acceptance_env_refused), and the var never reaches
// the invocation BaseEnv.
func TestRun_AcceptanceStage_RefusedPassthrough_LoggedAndAbsent(t *testing.T) {
	_, _, args := acceptanceStageSetup(t)
	t.Setenv("FISHHAWK_ACCEPTANCE_ENV_GITHUB_TOKEN", "ghp_smuggled")
	invoker := &fakeInvoker{canned: agent.Result{
		OK:               true,
		StructuredOutput: []byte(`{"verdict":"passed"}`),
	}}
	withFakeInvoker(t, invoker)

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"event":"acceptance_env_refused"`) ||
		!strings.Contains(stderr.String(), `GITHUB_TOKEN`) {
		t.Errorf("missing acceptance_env_refused log naming GITHUB_TOKEN: %s", stderr.String())
	}
	if invoker.gotInv == nil {
		t.Fatal("invocation not captured")
	}
	for _, kv := range invoker.gotInv.BaseEnv {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Errorf("refused passthrough leaked into BaseEnv: %s", kv)
		}
	}
}

// TestRun_AcceptanceStage_FileFallback_Ships is the codex path: no
// StructuredOutput (the backend ignores Invocation.JSONSchema), the
// agent wrote the verdict to the contracted fallback file instead — the
// runner picks it up and ships it.
func TestRun_AcceptanceStage_FileFallback_Ships(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	invoker := &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(_ int, _ agent.Invocation) {
			mustWrite(t, acceptanceVerdictPath,
				`{"verdict":"passed","criteria":[{"id":"AC1","result":"passed"}]}`)
		},
	}
	withFakeInvoker(t, invoker)

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("ShipAcceptance not called on the file-fallback path")
	}
	var shipped acceptanceVerdict
	if err := json.Unmarshal(fu.gotAcceptanceArgs.Body, &shipped); err != nil || shipped.Verdict != "passed" {
		t.Errorf("shipped verdict = %+v (err %v)", shipped, err)
	}
}

// TestRun_AcceptanceStage_FileFallback_NotesShips is a4 (#1567): the
// FALLBACK FILE — the transport that bypasses --json-schema entirely, so
// no schema is in play at all — carries a top-level notes field, and the
// runner validates + ships it end-to-end. This is the exact run-f7a4b71b
// hole: on this path a benign top-level overflow field must validate
// (declared) rather than fail closed.
func TestRun_AcceptanceStage_FileFallback_NotesShips(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	invoker := &fakeInvoker{
		canned: agent.Result{OK: true},
		onInvoke: func(_ int, _ agent.Invocation) {
			mustWrite(t, acceptanceVerdictPath,
				`{"verdict":"passed","criteria":[{"id":"AC1","result":"passed"}],"notes":"preview was slow to boot but all criteria passed"}`)
		},
	}
	withFakeInvoker(t, invoker)

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK (notes on the schemaless file path must validate):\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("ShipAcceptance not called on the notes-carrying file-fallback path")
	}
	var shipped acceptanceVerdict
	if err := json.Unmarshal(fu.gotAcceptanceArgs.Body, &shipped); err != nil {
		t.Fatalf("unmarshal shipped verdict: %v", err)
	}
	if shipped.Verdict != "passed" || shipped.Notes != "preview was slow to boot but all criteria passed" {
		t.Errorf("shipped verdict = %+v, want passed with notes preserved", shipped)
	}
}

// TestRun_AcceptanceStage_StaleFallbackCleared: a stale verdict left at
// the fixed fallback path by a PRIOR run must be removed BEFORE the
// acceptance agent runs. Here the agent produces neither StructuredOutput
// nor a fresh file; without the pre-run clear, captureAcceptanceVerdict
// would read and ship the stale bytes. With the clear (#1535 fix-up) the
// stage instead demotes to category-B acceptance_verdict_missing and
// nothing ships.
func TestRun_AcceptanceStage_StaleFallbackCleared(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	// Simulate a stale verdict from a previous run at the shared path.
	mustWrite(t, acceptanceVerdictPath,
		`{"verdict":"passed","criteria":[{"id":"AC1","result":"passed"}]}`)
	// The agent settles OK but writes NO fresh verdict (no StructuredOutput,
	// no onInvoke file write).
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{OK: true}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitFailure {
		t.Fatalf("run = %d, want exitFailure (stale verdict must not resurrect):\n%s", got, stderr.String())
	}
	out := stderr.String()
	if !strings.Contains(out, `"event":"acceptance_verdict_missing"`) {
		t.Errorf("stale fallback must be cleared → acceptance_verdict_missing, got: %s", out)
	}
	if fu.gotAcceptanceArgs != nil {
		t.Error("ShipAcceptance must not ship a prior run's stale verdict")
	}
}

// TestRun_AcceptanceStage_RedactionInShipAndBundle is the redaction
// done-means test: a credential-shaped string in criteria[].observed is
// redacted in BOTH the shipped body bytes and the acceptance_evidence
// event payload of BOTH bundle variants.
func TestRun_AcceptanceStage_RedactionInShipAndBundle(t *testing.T) {
	_, fu, args := acceptanceStageSetup(t)
	secret := "ghp_" + strings.Repeat("b", 36)
	withFakeInvoker(t, &fakeInvoker{canned: agent.Result{
		OK: true,
		StructuredOutput: []byte(
			`{"verdict":"passed","criteria":[{"id":"AC1","result":"passed","observed":"instance echoed ` + secret + `"}]}`),
	}})

	var stderr strings.Builder
	got := run(args, &stderr)
	if got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	if fu.gotAcceptanceArgs == nil {
		t.Fatal("ShipAcceptance not called")
	}
	if strings.Contains(string(fu.gotAcceptanceArgs.Body), secret) {
		t.Error("credential survived into the shipped acceptance body")
	}
	if !strings.Contains(string(fu.gotAcceptanceArgs.Body), "[REDACTED:") {
		t.Error("shipped body carries no redaction marker")
	}
	// Both uploaded bundle variants must carry the redacted evidence.
	if len(fu.gotShipCalls) != 2 {
		t.Fatalf("ShipTrace calls = %d, want 2 (raw + redacted)", len(fu.gotShipCalls))
	}
	for _, call := range fu.gotShipCalls {
		plain, err := gunzip(call.Bundle)
		if err != nil {
			t.Fatalf("gunzip %s bundle: %v", call.Variant, err)
		}
		if !strings.Contains(string(plain), "acceptance_evidence") {
			t.Errorf("%s bundle missing acceptance_evidence event", call.Variant)
		}
		if strings.Contains(string(plain), secret) {
			t.Errorf("credential survived into the %s bundle", call.Variant)
		}
	}
}
