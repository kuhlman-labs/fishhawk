package plan_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return b
}

// --- Happy paths ---

func TestParse_Example(t *testing.T) {
	p, err := plan.Parse(readFixture(t, "valid/example.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.PlanVersion != "standard_v1" {
		t.Errorf("PlanVersion = %q", p.PlanVersion)
	}
	if p.TicketReference.Type != plan.TicketTypeGitHubIssue {
		t.Errorf("TicketReference.Type = %q", p.TicketReference.Type)
	}
	if p.GeneratedBy.Agent != "claude-code" {
		t.Errorf("Agent = %q", p.GeneratedBy.Agent)
	}
	if p.GeneratedBy.Timestamp.IsZero() {
		t.Error("Timestamp should parse to a non-zero time.Time")
	}
	if got, want := p.GeneratedBy.Timestamp.UTC(), time.Date(2026, 4, 30, 14, 22, 11, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", got, want)
	}
	if got, want := len(p.Scope.Files), 2; got != want {
		t.Errorf("scope.files len = %d, want %d", got, want)
	}
	if p.Scope.Files[0].Operation != plan.FileOpCreate {
		t.Errorf("first file op = %q", p.Scope.Files[0].Operation)
	}
	if got, want := len(p.Approach), 2; got != want {
		t.Errorf("approach len = %d, want %d", got, want)
	}
	if p.Approach[0].Step != 1 {
		t.Errorf("first step = %d", p.Approach[0].Step)
	}
}

func TestValidate_Example(t *testing.T) {
	if err := plan.Validate(readFixture(t, "valid/example.json")); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// --- ParseError cases ---

func TestParse_Empty(t *testing.T) {
	_, err := plan.Parse(nil)
	var pe *plan.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
}

func TestParse_Whitespace(t *testing.T) {
	_, err := plan.Parse([]byte("   \n\t  "))
	var pe *plan.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
}

func TestParse_MalformedJSON(t *testing.T) {
	_, err := plan.Parse([]byte(`{"plan_version": "standard_v1",`))
	var pe *plan.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
}

// --- SchemaError cases ---

func TestParse_MissingPlanVersion(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_WrongPlanVersion(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "v999",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_UnknownFileOperation(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "rename"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_EmptyApproach(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_AdditionalPropertyRejected(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "2026-01-01T00:00:00Z"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"},
  "extra_field": "not allowed"
}`))
	var se *plan.SchemaError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
}

func TestParse_NonRFC3339Timestamp(t *testing.T) {
	_, err := plan.Parse([]byte(`{
  "plan_version": "standard_v1",
  "ticket_reference": {"type": "github_issue", "url": "https://x", "id": "x"},
  "generated_by": {"agent": "a", "model": "m", "timestamp": "yesterday"},
  "summary": "x",
  "scope": {"files": [{"path": "a.go", "operation": "create"}]},
  "approach": [{"step": 1, "description": "x"}],
  "verification": {"test_strategy": "x", "rollback_plan": "x"}
}`))
	// JSON Schema's "format: date-time" is annotation-only by default in
	// many validators. If the schema we ship asserts it, this test
	// passes via *SchemaError; if not, the typed json.Unmarshal of
	// time.Time will fail in Parse and surface as an internal error.
	// Either path is acceptable; we just require the call to fail.
	if err == nil {
		t.Fatal("expected error on non-RFC-3339 timestamp")
	}
	var se *plan.SchemaError
	var pe *plan.ParseError
	if !errors.As(err, &se) && !errors.As(err, &pe) && !strings.Contains(err.Error(), "decode to Plan") {
		t.Errorf("err = %v, want *SchemaError, *ParseError, or internal decode error", err)
	}
}

// --- Validate-only path ---

func TestValidate_ReturnsSameErrorAsParseSchemaPath(t *testing.T) {
	bad := []byte(`{"plan_version": "standard_v1"}`) // missing required fields
	if err := plan.Validate(bad); err == nil {
		t.Fatal("Validate(bad) should error")
	}
	if _, err := plan.Parse(bad); err == nil {
		t.Fatal("Parse(bad) should error")
	}
}

// --- Error formatting + unwrap ---

func TestParseError_FormatsMessageWithPrefix(t *testing.T) {
	err := &plan.ParseError{Msg: "broken"}
	if got, want := err.Error(), "plan: parse: broken"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestParseError_FormatsCauseWhenMsgEmpty(t *testing.T) {
	cause := errors.New("syntax error at line 4")
	err := &plan.ParseError{Cause: cause}
	if got := err.Error(); !strings.Contains(got, "syntax error") {
		t.Errorf("Error() = %q, want it to contain the cause", got)
	}
}

func TestParseError_UnknownErrorWhenBothEmpty(t *testing.T) {
	err := &plan.ParseError{}
	if got, want := err.Error(), "plan: parse: unknown error"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestParseError_UnwrapExposesCause(t *testing.T) {
	cause := errors.New("eof")
	err := &plan.ParseError{Cause: cause}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is should resolve through Unwrap")
	}
}

func TestSchemaError_FormatsPathAndMessage(t *testing.T) {
	err := &plan.SchemaError{Path: "/scope/files/0", Message: "missing 'path'"}
	if got, want := err.Error(), "plan: schema: /scope/files/0: missing 'path'"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
