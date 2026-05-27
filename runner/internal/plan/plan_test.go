package plan

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/plan/planfixture"
)

func fixtureJSON(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidate_Valid(t *testing.T) {
	if err := Validate(fixtureJSON(t, planfixture.Valid())); err != nil {
		t.Errorf("Validate: unexpected error: %v", err)
	}
}

func TestValidate_EmptyDocument(t *testing.T) {
	var perr *ParseError
	if err := Validate(nil); !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
	if err := Validate([]byte("   ")); !errors.As(err, &perr) {
		t.Fatalf("whitespace err = %v, want *ParseError", err)
	}
}

func TestValidate_MalformedJSON(t *testing.T) {
	var perr *ParseError
	err := Validate([]byte("{ not json"))
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *ParseError", err)
	}
	if perr.Cause == nil {
		t.Error("expected ParseError.Cause to be set")
	}
}

func TestValidate_MissingRequired(t *testing.T) {
	// Drop the `summary` field — schema requires it.
	src := fixtureJSON(t, planfixture.Valid())
	tampered := strings.Replace(string(src), `"summary":`, `"summary_renamed":`, 1)
	var serr *SchemaError
	err := Validate([]byte(tampered))
	if !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if !strings.Contains(serr.Error(), "schema violation") {
		t.Errorf("error message missing 'schema violation': %v", serr)
	}
}

func TestValidate_BadEnum(t *testing.T) {
	// scope.files[].operation only accepts a closed set; "demolish"
	// is not a member.
	src := fixtureJSON(t, planfixture.Valid())
	tampered := strings.Replace(string(src), `"create"`, `"demolish"`, 1)
	var serr *SchemaError
	err := Validate([]byte(tampered))
	if !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	if serr.Path == "" {
		t.Errorf("SchemaError.Path is empty: %+v", serr)
	}
}

func TestValidate_BadVersion(t *testing.T) {
	// plan_version is constrained to "standard_v1".
	src := fixtureJSON(t, planfixture.Valid())
	tampered := strings.Replace(string(src), `"standard_v1"`, `"standard_v2"`, 1)
	if err := Validate([]byte(tampered)); err == nil {
		t.Fatal("expected version error")
	}
}

func TestSchemaErrorFormatting(t *testing.T) {
	// Branch coverage for the no-path error fallback.
	se := &SchemaError{Path: "", Message: "bad"}
	if !strings.Contains(se.Error(), "bad") {
		t.Errorf("SchemaError.Error missing message: %s", se.Error())
	}
	se2 := &SchemaError{Path: "/scope/files/0/operation", Message: "not in enum"}
	if !strings.Contains(se2.Error(), "/scope/files/0/operation") {
		t.Errorf("SchemaError.Error missing path: %s", se2.Error())
	}
}

func TestSchemaError_MultiViolation(t *testing.T) {
	// approach must be an array; verification must be an object.
	// Both as bare strings should produce violations at both paths.
	m := planfixture.Valid(func(m map[string]any) {
		m["approach"] = "should be an array"
		m["verification"] = "should be an object"
	})
	var serr *SchemaError
	if err := Validate(fixtureJSON(t, m)); !errors.As(err, &serr) {
		t.Fatalf("err = %v, want *SchemaError", err)
	}
	hasApproach := false
	hasVerification := false
	for _, v := range serr.Violations {
		if v.Path == "/approach" {
			hasApproach = true
		}
		if v.Path == "/verification" {
			hasVerification = true
		}
	}
	if !hasApproach {
		t.Errorf("Violations missing /approach entry; got %+v", serr.Violations)
	}
	if !hasVerification {
		t.Errorf("Violations missing /verification entry; got %+v", serr.Violations)
	}
}

func TestParseErrorUnwrap(t *testing.T) {
	cause := errors.New("underlying")
	pe := &ParseError{Msg: "wrapped", Cause: cause}
	if errors.Unwrap(pe) != cause {
		t.Errorf("Unwrap returned %v, want %v", errors.Unwrap(pe), cause)
	}
}
