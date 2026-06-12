package operatorrole

import (
	"errors"
	"strings"
	"testing"
)

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// TestDefaultRoundTrip exercises the embed → YAML → schema → typed
// struct path end-to-end: package init has already validated the
// embedded default against the embedded full schema (a failure panics
// the test binary), so here we assert the typed result carries every
// section.
func TestDefaultRoundTrip(t *testing.T) {
	d := Default()

	if d.Role != "operator" {
		t.Errorf("Role = %q, want %q", d.Role, "operator")
	}
	if d.SpecVersion != "operator-role-v0" {
		t.Errorf("SpecVersion = %q, want %q", d.SpecVersion, "operator-role-v0")
	}
	if d.Mission == "" {
		t.Error("Mission is empty")
	}

	procedures := map[string][]string{
		"pre_flight":            d.GateProcedures.PreFlight,
		"plan_gate":             d.GateProcedures.PlanGate,
		"implement_review_gate": d.GateProcedures.ImplementReviewGate,
		"merge_ritual":          d.GateProcedures.MergeRitual,
		"recovery":              d.GateProcedures.Recovery,
	}
	for name, steps := range procedures {
		if len(steps) == 0 {
			t.Errorf("gate_procedures.%s is empty", name)
		}
		for i, s := range steps {
			if s == "" {
				t.Errorf("gate_procedures.%s[%d] is empty", name, i)
			}
		}
	}

	if len(d.Escalation.AlwaysPage) == 0 {
		t.Error("Escalation.AlwaysPage is empty")
	}
	if d.Escalation.PageFormat == "" {
		t.Error("Escalation.PageFormat is empty")
	}
	if len(d.Conventions) == 0 {
		t.Error("Conventions is empty")
	}
	if len(d.Forbidden) == 0 {
		t.Error("Forbidden is empty")
	}
}

// TestValidateOverlayValid mirrors the documented example overlay
// (docs/spec/examples/operator-role-overlay-example.yaml).
func TestValidateOverlayValid(t *testing.T) {
	overlay := `
spec_version: operator-role-v0
knob_presets:
  autonomy: medium
conventions:
  escalation_contact: "page @kuhlman-labs in the driving session first"
  merge_ritual_local: squash merges only; delete the run branch after merge
work_management: github-project:kuhlman-labs/7
`
	if err := ValidateOverlay(strings.NewReader(overlay)); err != nil {
		t.Fatalf("ValidateOverlay(valid overlay) = %v, want nil", err)
	}
}

func TestValidateOverlayMinimal(t *testing.T) {
	if err := ValidateOverlay(strings.NewReader("spec_version: operator-role-v0\n")); err != nil {
		t.Fatalf("ValidateOverlay(minimal overlay) = %v, want nil", err)
	}
}

// TestValidateOverlayThinness covers every structurally excluded
// procedure field: each must fail with a *ThinnessError naming the
// field and the rule.
func TestValidateOverlayThinness(t *testing.T) {
	cases := map[string]string{
		"gate_procedures": `
spec_version: operator-role-v0
gate_procedures:
  extra_gate:
    - a per-repo procedure step
`,
		"mission": `
spec_version: operator-role-v0
mission: a repo-local mission statement
`,
		"escalation": `
spec_version: operator-role-v0
escalation:
  page_format: terse
`,
		"forbidden": `
spec_version: operator-role-v0
forbidden:
  - a repo-local prohibition
`,
	}
	for field, overlay := range cases {
		t.Run(field, func(t *testing.T) {
			err := ValidateOverlay(strings.NewReader(overlay))
			if err == nil {
				t.Fatalf("ValidateOverlay accepted overlay setting %q", field)
			}
			var thin *ThinnessError
			if !errors.As(err, &thin) {
				t.Fatalf("error = %v (%T), want *ThinnessError", err, err)
			}
			if thin.Field != field {
				t.Errorf("ThinnessError.Field = %q, want %q", thin.Field, field)
			}
			for _, want := range []string{field, "thinness rule", "file an issue"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error message %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}

// TestValidateOverlayRejects covers non-thinness structural failures:
// these must come back as *SchemaError, not *ThinnessError.
func TestValidateOverlayRejects(t *testing.T) {
	cases := map[string]string{
		"unknown top-level key": `
spec_version: operator-role-v0
playbook_extras: nope
`,
		"missing spec_version": `
knob_presets:
  autonomy: medium
`,
		"wrong spec_version": `
spec_version: operator-role-v9
`,
		"non-string knob preset": `
spec_version: operator-role-v0
knob_presets:
  autonomy: 3
`,
		"non-object document": `just a string`,
	}
	for name, overlay := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateOverlay(strings.NewReader(overlay))
			if err == nil {
				t.Fatal("ValidateOverlay accepted an invalid overlay")
			}
			var serr *SchemaError
			if !errors.As(err, &serr) {
				t.Fatalf("error = %v (%T), want *SchemaError", err, err)
			}
		})
	}
}

// TestValidateOverlayMultiDocument locks the multi-document bypass: a
// stream whose first document is valid must not smuggle anything —
// least of all procedure fields — in trailing documents. The whole
// stream is rejected as a *YAMLError telling the operator the file
// must be a single document.
func TestValidateOverlayMultiDocument(t *testing.T) {
	cases := map[string]string{
		"procedure field in second document": `
spec_version: operator-role-v0
---
gate_procedures:
  extra_gate:
    - a smuggled per-repo procedure step
`,
		"valid second document": `
spec_version: operator-role-v0
---
spec_version: operator-role-v0
`,
		"malformed second document": `
spec_version: operator-role-v0
---
spec_version: [unclosed
`,
	}
	for name, overlay := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateOverlay(strings.NewReader(overlay))
			if err == nil {
				t.Fatal("ValidateOverlay accepted a multi-document stream")
			}
			var yerr *YAMLError
			if !errors.As(err, &yerr) {
				t.Fatalf("error = %v (%T), want *YAMLError", err, err)
			}
			if !strings.Contains(err.Error(), "single document") {
				t.Errorf("error message %q does not name the single-document requirement", err.Error())
			}
		})
	}
}

func TestValidateOverlayYAMLErrors(t *testing.T) {
	cases := map[string]string{
		"empty document":  "",
		"whitespace only": "   \n\t\n",
		"malformed yaml":  "spec_version: [unclosed",
	}
	for name, overlay := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateOverlay(strings.NewReader(overlay))
			if err == nil {
				t.Fatal("ValidateOverlay accepted unparseable input")
			}
			var yerr *YAMLError
			if !errors.As(err, &yerr) {
				t.Fatalf("error = %v (%T), want *YAMLError", err, err)
			}
		})
	}
}

func TestEmbeddedSchemaHash(t *testing.T) {
	h := EmbeddedSchemaHash()
	if len(h) != 64 {
		t.Fatalf("EmbeddedSchemaHash() = %q, want 64 hex chars", h)
	}
	if strings.Trim(h, "0123456789abcdef") != "" {
		t.Fatalf("EmbeddedSchemaHash() = %q, want lowercase hex", h)
	}
	if again := EmbeddedSchemaHash(); again != h {
		t.Fatalf("EmbeddedSchemaHash() not stable: %q then %q", h, again)
	}
}

func TestValidateOverlayReadError(t *testing.T) {
	readErr := errors.New("disk on fire")
	err := ValidateOverlay(errReader{err: readErr})
	if !errors.Is(err, readErr) {
		t.Fatalf("ValidateOverlay(failing reader) = %v, want wrapped %v", err, readErr)
	}
}

func TestErrorMessages(t *testing.T) {
	cause := errors.New("line 3: bad indent")
	yWithMsg := &YAMLError{Msg: "empty document"}
	if got := yWithMsg.Error(); !strings.Contains(got, "empty document") {
		t.Errorf("YAMLError.Error() = %q, want it to contain the message", got)
	}
	yWithCause := &YAMLError{Cause: cause}
	if got := yWithCause.Error(); !strings.Contains(got, cause.Error()) {
		t.Errorf("YAMLError.Error() = %q, want it to contain the cause", got)
	}
	if !errors.Is(yWithCause, cause) {
		t.Error("YAMLError does not unwrap to its cause")
	}
	yEmpty := &YAMLError{}
	if got := yEmpty.Error(); !strings.Contains(got, "unknown error") {
		t.Errorf("YAMLError.Error() = %q, want the unknown-error fallback", got)
	}

	serr := &SchemaError{Path: "/knob_presets/autonomy", Message: "got number, want string"}
	for _, want := range []string{serr.Path, serr.Message} {
		if !strings.Contains(serr.Error(), want) {
			t.Errorf("SchemaError.Error() = %q, want it to contain %q", serr.Error(), want)
		}
	}
}

func TestStringListUnmarshal(t *testing.T) {
	var s StringList
	if err := s.UnmarshalJSON([]byte(`"one condition"`)); err != nil {
		t.Fatalf("UnmarshalJSON(string) = %v", err)
	}
	if len(s) != 1 || s[0] != "one condition" {
		t.Errorf("StringList from string = %v, want [one condition]", s)
	}
	if err := s.UnmarshalJSON([]byte(`["a","b"]`)); err != nil {
		t.Fatalf("UnmarshalJSON(list) = %v", err)
	}
	if len(s) != 2 || s[0] != "a" || s[1] != "b" {
		t.Errorf("StringList from list = %v, want [a b]", s)
	}
	if err := s.UnmarshalJSON([]byte(`42`)); err == nil {
		t.Error("UnmarshalJSON(42) = nil, want error")
	}
}
