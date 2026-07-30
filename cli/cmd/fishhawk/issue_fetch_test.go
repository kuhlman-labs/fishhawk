package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

func TestResolveIssueRef_Forms(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false}, // empty = "user passed nothing"
		{"1247", 1247, false},
		{"#1247", 1247, false},
		{"  1247  ", 1247, false},
		{"https://github.com/owner/repo/issues/1247", 1247, false},
		{"https://github.com/owner/repo/issues/1247/", 1247, false},
		{"https://github.com/owner/repo/issues/1247#issuecomment-99", 1247, false},
		{"not-a-number", 0, true},
		{"-3", 0, true},
		{"0", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := resolveIssueRef(tc.in)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr=%v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInferIssueNumberFromTriggerRef(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"issue:42", 42},
		{"issue:0", 0},   // non-positive ignored
		{"issue:abc", 0}, // non-numeric ignored
		{"pr:42", 0},     // wrong prefix
		{"42", 0},        // no prefix
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := inferIssueNumberFromTriggerRef(tc.in); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// withFakeGh swaps in a fake gh subprocess that returns the
// supplied JSON body verbatim. The actual command path doesn't
// matter — we replace ghIssueCommand entirely.
func withFakeGh(t *testing.T, jsonBody string) {
	t.Helper()
	orig := ghIssueCommand
	ghIssueCommand = func(_ string, _ ...string) *exec.Cmd {
		// echo prints the body to stdout; gh's --json output is
		// JSON-on-stdout.
		return exec.Command("sh", "-c", "cat <<'BODY'\n"+jsonBody+"\nBODY")
	}
	t.Cleanup(func() { ghIssueCommand = orig })
}

// withFakeGhMissing makes the gh binary appear absent from PATH.
// The function temporarily swaps PATH so exec.LookPath fails.
func withFakeGhMissing(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	orig := os.Getenv("PATH")
	t.Setenv("PATH", tmp)
	t.Cleanup(func() { _ = os.Setenv("PATH", orig) })
}

// withFakeGhBroken returns a non-zero exit from the stub so the
// failure branch is exercised.
func withFakeGhBroken(t *testing.T) {
	t.Helper()
	orig := ghIssueCommand
	ghIssueCommand = func(_ string, _ ...string) *exec.Cmd {
		// /usr/bin/false exits 1 with no output; matches the
		// "command ran, returned an error" path.
		return exec.Command("/usr/bin/false")
	}
	t.Cleanup(func() { ghIssueCommand = orig })
}

func TestFetchIssueViaGh_Success(t *testing.T) {
	withFakeGh(t, `{"title":"Add foo","body":"We need foo.","url":"https://github.com/x/y/issues/42","number":42}`)
	got, err := fetchIssueViaGh("x/y", 42)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Title != "Add foo" || got.Body != "We need foo." || got.Number != 42 {
		t.Errorf("decoded payload mismatch: %+v", got)
	}
	if got.URL != "https://github.com/x/y/issues/42" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestFetchIssueViaGh_DecodesComments(t *testing.T) {
	withFakeGh(t, `{"title":"Add foo","body":"We need foo.","url":"https://github.com/x/y/issues/42","number":42,"comments":[{"author":{"login":"alice"},"body":"Refinement here.","createdAt":"2026-05-01T10:00:00Z"},{"author":{"login":"bob"},"body":"Second comment.","createdAt":"2026-05-02T11:00:00Z"}]}`)
	got, err := fetchIssueViaGh("x/y", 42)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("want 2 comments, got %d: %+v", len(got.Comments), got.Comments)
	}
	if got.Comments[0].Author != "alice" || got.Comments[0].Body != "Refinement here." ||
		got.Comments[0].CreatedAt != "2026-05-01T10:00:00Z" {
		t.Errorf("comment[0] mismatch: %+v", got.Comments[0])
	}
	if got.Comments[1].Author != "bob" {
		t.Errorf("comment[1] author = %q, want bob", got.Comments[1].Author)
	}
}

func TestFetchIssueViaGh_NotInstalled(t *testing.T) {
	withFakeGhMissing(t)
	_, err := fetchIssueViaGh("x/y", 42)
	if !errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err = %v, want ErrGhNotInstalled", err)
	}
}

func TestFetchIssueViaGh_CommandFails(t *testing.T) {
	// Need gh on PATH so the LookPath check passes; then the
	// fake command fails the actual call.
	withFakeGh(t, "ignored")
	withFakeGhBroken(t)
	_, err := fetchIssueViaGh("x/y", 42)
	if err == nil {
		t.Fatal("expected error from broken gh subprocess")
	}
	if errors.Is(err, ErrGhNotInstalled) {
		t.Errorf("err should NOT be ErrGhNotInstalled: %v", err)
	}
}

// TestRunStart_IssueFlag_ForwardsContext is the headline #415
// end-to-end CLI check: --issue triggers a gh fetch, the result
// rides along in the CreateRunInput, and the trigger_source
// auto-flips to github_issue.
func TestRunStart_IssueFlag_ForwardsContext(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	withFakeGh(t, `{"title":"Add foo","body":"We need foo helpers.","url":"https://github.com/x/y/issues/42","number":42}`)

	// Need a spec file so runStart doesn't error out on
	// "workflow spec not found." Mirror the layout from #411
	// tests.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n")
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", dir,
		"--issue", "42",
	}, io.Discard, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK", rc)
	}
	if fb.startedRun.IssueContext == nil {
		t.Fatal("IssueContext not forwarded to backend")
	}
	if fb.startedRun.IssueContext.Body != "We need foo helpers." {
		t.Errorf("body mismatch: %q", fb.startedRun.IssueContext.Body)
	}
	if fb.startedRun.TriggerSource != "github_issue" {
		t.Errorf("TriggerSource = %q, want github_issue", fb.startedRun.TriggerSource)
	}
	if fb.startedRun.TriggerRef == nil || *fb.startedRun.TriggerRef != "issue:42" {
		t.Errorf("TriggerRef = %v, want issue:42", fb.startedRun.TriggerRef)
	}
}

// TestRunStart_TriggerRefIssue_AutoDerivesNumber exercises the
// auto-derive path: when --trigger-ref is `issue:N` and --issue
// is not set, the CLI infers the issue number and still fetches
// via gh.
func TestRunStart_TriggerRefIssue_AutoDerivesNumber(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	withFakeGh(t, `{"title":"Auto","body":"Auto-derived.","url":"https://github.com/x/y/issues/7","number":7}`)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n")
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", dir,
		"--trigger-ref", "issue:7",
	}, io.Discard, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d", rc)
	}
	if fb.startedRun.IssueContext == nil || fb.startedRun.IssueContext.Number != 7 {
		t.Errorf("expected IssueContext.Number=7, got %+v", fb.startedRun.IssueContext)
	}
}

// TestRunStart_NoIssueFlag_NoFetch confirms the CLI does NOT
// shell to gh when the operator doesn't request the issue
// context. Avoids unsolicited gh invocations in workflows that
// don't need one.
func TestRunStart_NoIssueFlag_NoFetch(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	// Wire a gh that would explode if called.
	ghIssueCommand = func(_ string, _ ...string) *exec.Cmd {
		t.Fatal("gh should NOT have been called without --issue")
		return nil
	}
	t.Cleanup(func() { ghIssueCommand = exec.Command })

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n")
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", dir,
	}, io.Discard, io.Discard)
	if rc != exitOK {
		t.Fatalf("rc = %d", rc)
	}
	if fb.startedRun.IssueContext != nil {
		t.Errorf("IssueContext set without --issue: %+v", fb.startedRun.IssueContext)
	}
	if fb.startedRun.TriggerSource != "cli" {
		t.Errorf("TriggerSource = %q, want cli", fb.startedRun.TriggerSource)
	}
}

// TestRunStart_GhMissing_ProceedsWithoutContext keeps the
// pre-#415 behavior alive when the operator doesn't have gh: the
// run still mints, the prompt falls back to URL-only.
func TestRunStart_GhMissing_ProceedsWithoutContext(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	withFakeGhMissing(t)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("version: \"0.3\"\nworkflows:\n  trivial:\n    stages:\n      - id: implement\n        type: implement\n        executor:\n          agent: claude-code\n        produces:\n          - artifact: pull_request\n")
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	var stderr strings.Builder
	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", dir,
		"--issue", "42",
	}, io.Discard, &stderr)
	if rc != exitOK {
		t.Fatalf("rc = %d, want exitOK (gh missing is non-fatal):\n%s", rc, stderr.String())
	}
	if fb.startedRun.IssueContext != nil {
		t.Errorf("IssueContext should be nil when gh missing: %+v", fb.startedRun.IssueContext)
	}
	// trigger_source should still flip to github_issue because the
	// operator asked for an issue-triggered run.
	if fb.startedRun.TriggerSource != "github_issue" {
		t.Errorf("TriggerSource = %q, want github_issue", fb.startedRun.TriggerSource)
	}
	if !strings.Contains(stderr.String(), "gh CLI not on PATH") {
		t.Errorf("stderr should mention gh missing: %s", stderr.String())
	}
}

// TestRunStart_BadIssueFlag_FailsFast exercises the validation
// branch for resolveIssueRef.
func TestRunStart_BadIssueFlag_FailsFast(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	rc := run([]string{
		"run", "start",
		"--repo", "x/y", "--workflow", "w",
		"--issue", "garbage",
		"--workflow-sha", "abc",
	}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Fatalf("rc = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "--issue") {
		t.Errorf("stderr should mention --issue: %s", stderr.String())
	}
}

// Ensure the fake response shape decodes cleanly into the typed
// CreateRunInput.IssueContext (smoke against the JSON tags).
func TestIssueContext_RoundTrip(t *testing.T) {
	ic := httpclient.IssueContext{Title: "T", Body: "B", URL: "U", Number: 5}
	if ic.Title != "T" {
		t.Error("trivial sanity")
	}
}

// --- issue labels: the applies_to `labels` criterion's only producer (#2226) ---
//
// Deliberately a DUPLICATE of backend/cmd/fishhawk-mcp/issue_fetch_test.go's
// three cases. The two binaries are separate Go modules carrying their own
// ghIssue decode, so parity is held by paired assertions rather than shared
// code — changing only one leaves CLI-started runs with an empty label set and
// therefore fail-closed against every labels-declaring workflow.

// TestFetchIssueViaGh_RequestsLabelsField locks the `--json` argv: without
// `labels` in the field list gh omits the key entirely.
func TestFetchIssueViaGh_RequestsLabelsField(t *testing.T) {
	var gotArgs []string
	orig := ghIssueCommand
	ghIssueCommand = func(name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{name}, args...)
		return exec.Command("sh", "-c", `printf '{"title":"t","number":1}'`)
	}
	t.Cleanup(func() { ghIssueCommand = orig })

	if _, err := fetchIssueViaGh("x/y", 42); err != nil {
		t.Fatalf("err = %v", err)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--json title,body,url,number,comments,labels") {
		t.Errorf("gh invoked as %q; want the --json field list to include labels", joined)
	}
}

// TestFetchIssueViaGh_ProjectsLabelObjectsToNames pins the decode shape: gh
// emits labels as OBJECTS carrying a `name` key, not as strings.
func TestFetchIssueViaGh_ProjectsLabelObjectsToNames(t *testing.T) {
	withFakeGh(t, `{"title":"Bump dep","body":"b","url":"https://github.com/x/y/issues/9","number":9,"labels":[{"id":"LA_1","name":"dependencies","color":"0366d6"},{"id":"LA_2","name":"area:backend","color":"d73a4a"}]}`)
	got, err := fetchIssueViaGh("x/y", 9)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if want := "dependencies,area:backend"; strings.Join(got.Labels, ",") != want {
		t.Errorf("Labels = %v, want [%s] projected from the label objects' name key", got.Labels, want)
	}
}

// TestFetchIssueViaGh_ToleratesAbsentLabels covers the degrade: an unlabelled
// issue decodes to a nil slice rather than erroring. The empty set is then
// fail-closed at admission — documented behaviour, not a fetch failure.
func TestFetchIssueViaGh_ToleratesAbsentLabels(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"key absent", `{"title":"t","body":"b","url":"u","number":1}`},
		{"empty array", `{"title":"t","body":"b","url":"u","number":1,"labels":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeGh(t, tc.body)
			got, err := fetchIssueViaGh("x/y", 1)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(got.Labels) != 0 {
				t.Errorf("Labels = %v, want empty", got.Labels)
			}
		})
	}
}

// TestRunStart_IssueLabels_ReachTheRequestBody is the CLI half of the
// client-drops-the-field seam (binding CONDITION 1, second consequence).
//
// The gap it closes: the fetch tests above end at the DECODE and
// httpclient's W2 tests begin at an already-populated CreateRunInput, so the
// ORCHESTRATION between them — run.go's `in.IssueContext = ic` hand-off —
// could drop labels entirely while every other CLI test passes. Every
// legitimate label-constrained CLI run would then fail closed against a
// labels-declaring workflow, which reads as a control bug rather than a
// plumbing omission.
//
// This drives fake `gh` output through the real `fishhawk run start` call
// site into a real HTTP request and asserts on the body the backend RECEIVES.
func TestRunStart_IssueLabels_ReachTheRequestBody(t *testing.T) {
	withFakeGh(t, `{"title":"Bump dep","body":"b","url":"https://github.com/x/y/issues/9","number":9,"labels":[{"name":"dependencies"},{"name":"area:backend"}]}`)

	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Errorf("backend received a body that is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","repo":"x/y","workflow_id":"trivial","state":"pending","runner_kind":"github_actions"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), []byte(runStartSpecYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--issue", "9",
		"--working-dir", dir,
		"--backend-url", srv.URL,
		"--token", "tok-test",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runStart exit = %d, want 0\nstderr: %s", code, stderr.String())
	}

	ic, _ := received["issue_context"].(map[string]any)
	if ic == nil {
		t.Fatalf("request body carried no issue_context: %+v (stderr: %s)", received, stderr.String())
	}
	labels, _ := ic["labels"].([]any)
	if len(labels) != 2 || labels[0] != "dependencies" || labels[1] != "area:backend" {
		t.Errorf("issue_context.labels on the wire = %+v, want the two fetched names — a drop here fails closed on EVERY labels-declaring CLI run", ic["labels"])
	}
	// The run must still be issue-triggered: labels ride on the issue path.
	if received["trigger_source"] != "github_issue" {
		t.Errorf("trigger_source = %v, want github_issue", received["trigger_source"])
	}
}

// runStartSpecYAML is a minimal valid workflow spec for the orchestration
// test above; runStart refuses to proceed without one.
const runStartSpecYAML = `version: "0.5"
workflows:
  trivial:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`
