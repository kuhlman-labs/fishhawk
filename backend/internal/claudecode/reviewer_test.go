package claudecode

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// TestHelperProcess is the Go stdlib test-helper-process pattern: when
// GO_HELPER_PROCESS=1 is set, this test pretends to be the `claude` binary and
// emits a canned --output-format json envelope driven by HELPER_MODE. The real
// tests re-exec the test binary itself in place of the missing `claude`.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_HELPER_PROCESS") != "1" {
		return
	}
	defer os.Exit(0)

	switch os.Getenv("HELPER_MODE") {
	case "happy":
		// A success envelope whose result field is the verdict JSON.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"approve\"}"}`)
	case "error":
		// Non-zero exit stands in for a subprocess failure.
		fmt.Fprintln(os.Stderr, "claude: model rate-limited")
		os.Exit(1)
	case "non_json_result":
		// Valid envelope but the result text is not JSON.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"this is not json"}`)
	case "bad_verdict":
		// Valid envelope, valid JSON, but verdict outside the closed set.
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"{\"verdict\":\"maybe\"}"}`)
	default:
		fmt.Fprintln(os.Stderr, "unknown HELPER_MODE")
		os.Exit(2)
	}
}

// helperCommand returns a Cmd-builder that re-execs the test binary as the
// `claude` stand-in, passing through HELPER_MODE.
func helperCommand(mode string) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess")
		c.Env = append(os.Environ(),
			"GO_HELPER_PROCESS=1",
			"HELPER_MODE="+mode,
		)
		return c
	}
}

func testConfig() Config {
	return Config{
		Binary:    "claude",
		Model:     "claude-sonnet-4-6",
		MaxTokens: 4096,
		Timeout:   5 * time.Second,
	}
}

func reviewerWithMode(mode string) *Reviewer {
	r := NewReviewer(testConfig())
	r.client.Cmd = helperCommand(mode)
	return r
}

// TestReviewer_HappyPath asserts a success envelope decodes to an approve
// verdict and the returned model is the configured model.
func TestReviewer_HappyPath(t *testing.T) {
	verdict, model, err := reviewerWithMode("happy").Review(context.Background(), "review this plan")
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Verdict != planreview.VerdictApprove {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, planreview.VerdictApprove)
	}
	if model != "claude-sonnet-4-6" {
		t.Errorf("model = %q, want %q", model, "claude-sonnet-4-6")
	}
}

// TestReviewer_SubprocessError asserts a non-zero exit surfaces as an error.
func TestReviewer_SubprocessError(t *testing.T) {
	_, _, err := reviewerWithMode("error").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from non-zero subprocess exit, got nil")
	}
}

// TestReviewer_NonJSONResult asserts a result field that is not JSON surfaces
// as an error.
func TestReviewer_NonJSONResult(t *testing.T) {
	_, _, err := reviewerWithMode("non_json_result").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from non-JSON result text, got nil")
	}
}

// TestReviewer_UnknownVerdict asserts a valid envelope carrying a verdict
// outside the closed set surfaces as an error.
func TestReviewer_UnknownVerdict(t *testing.T) {
	_, _, err := reviewerWithMode("bad_verdict").Review(context.Background(), "review this plan")
	if err == nil {
		t.Fatal("expected error from unknown verdict value, got nil")
	}
}
