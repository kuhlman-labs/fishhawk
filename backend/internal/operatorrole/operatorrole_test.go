package operatorrole

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/specialistjob"
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
	// The campaign-scoped operator procedure (ADR-047 / #1450) lives as a
	// conventions entry, not a gate_procedures key (thinness rule). Assert
	// it is present and non-empty so a no-op/comment-only touch of the
	// shipped default fails the done-means.
	if strings.TrimSpace(d.Conventions["campaign"]) == "" {
		t.Error(`Conventions["campaign"] is absent or empty`)
	}
	if len(d.Forbidden) == 0 {
		t.Error("Forbidden is empty")
	}

	// The orchestration remit (ADR-097 D1, #4301) is optional in the schema
	// but always present in the shipped default. Assert it on the EMBEDDED
	// default so a comment-only or no-op touch of the YAML fails.
	o := d.Orchestration
	if o == nil {
		t.Fatal("Orchestration is nil: the shipped default must carry the remit")
	}
	if len(o.Remit) == 0 {
		t.Error("Orchestration.Remit is empty")
	}
	if len(o.ReadingResults) == 0 {
		t.Error("Orchestration.ReadingResults is empty")
	}
	for i, s := range append(append([]string{}, o.Remit...), o.ReadingResults...) {
		if strings.TrimSpace(s) == "" {
			t.Errorf("orchestration step %d is blank", i)
		}
	}
	// The delegation key set must EQUAL the specialist-job kind set, so a
	// kind added to specialistjob.AllKinds without a recipe fails here.
	var wantKinds, gotKinds []string
	for _, k := range specialistjob.AllKinds {
		wantKinds = append(wantKinds, string(k))
	}
	for k, r := range o.Delegation {
		gotKinds = append(gotKinds, k)
		if strings.TrimSpace(r.When) == "" {
			t.Errorf("Orchestration.Delegation[%q].When is empty", k)
		}
		if strings.TrimSpace(r.Read) == "" {
			t.Errorf("Orchestration.Delegation[%q].Read is empty", k)
		}
	}
	sort.Strings(wantKinds)
	sort.Strings(gotKinds)
	if strings.Join(gotKinds, ",") != strings.Join(wantKinds, ",") {
		t.Errorf("Orchestration.Delegation keys = %v, want exactly the specialist-job kinds %v", gotKinds, wantKinds)
	}
}

// defaultAsRawMap decodes the embedded default into a mutable generic
// document, the shape the full-schema fixtures below edit.
func defaultAsRawMap(t *testing.T) map[string]any {
	t.Helper()
	data, err := embedded.ReadFile(defaultSpecPath)
	if err != nil {
		t.Fatalf("read embedded default: %v", err)
	}
	var raw map[string]any
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&raw); err != nil {
		t.Fatalf("decode embedded default: %v", err)
	}
	// Round-trip through JSON so every nested value has the generic shape
	// the schema validator expects (yaml.v3 yields map[string]any already,
	// but this also normalizes numbers and guards against aliasing).
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal embedded default: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal embedded default: %v", err)
	}
	return out
}

// TestRoleSchemaClosesDelegationKinds pins the full schema's orchestration
// shape (ADR-097 D1): the delegation key set is closed to the six
// specialist-job kinds in BOTH directions, and the section stays optional
// within operator-role-v0.
func TestRoleSchemaClosesDelegationKinds(t *testing.T) {
	// Sanity: the untouched default validates, so each failure below is
	// attributable to its mutation alone.
	if err := roleSchema.Validate(defaultAsRawMap(t)); err != nil {
		t.Fatalf("embedded default does not validate: %v", err)
	}

	delegationOf := func(doc map[string]any) map[string]any {
		return doc["orchestration"].(map[string]any)["delegation"].(map[string]any)
	}

	t.Run("missing kind", func(t *testing.T) {
		doc := defaultAsRawMap(t)
		delete(delegationOf(doc), "draft")
		if err := roleSchema.Validate(doc); err == nil {
			t.Error("schema accepted a delegation block missing the draft kind")
		}
	})
	t.Run("unknown kind", func(t *testing.T) {
		doc := defaultAsRawMap(t)
		delegationOf(doc)["bogus_kind"] = map[string]any{"when": "w", "read": "r"}
		if err := roleSchema.Validate(doc); err == nil {
			t.Error("schema accepted a delegation block with an unknown kind key")
		}
	})
	t.Run("recipe missing read", func(t *testing.T) {
		doc := defaultAsRawMap(t)
		delegationOf(doc)["inventory"] = map[string]any{"when": "w"}
		if err := roleSchema.Validate(doc); err == nil {
			t.Error("schema accepted a delegation recipe without read")
		}
	})
	t.Run("blank recipe when", func(t *testing.T) {
		doc := defaultAsRawMap(t)
		delegationOf(doc)["inventory"] = map[string]any{"when": "", "read": "r"}
		if err := roleSchema.Validate(doc); err == nil {
			t.Error("schema accepted a delegation recipe with an empty when")
		}
	})
	t.Run("orchestration is optional within v0", func(t *testing.T) {
		doc := defaultAsRawMap(t)
		delete(doc, "orchestration")
		if err := roleSchema.Validate(doc); err != nil {
			t.Errorf("schema rejected a spec with no orchestration section (additive optional within v0): %v", err)
		}
	})
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
	cases := []struct {
		name    string
		field   string
		overlay string
	}{
		{"gate_procedures", "gate_procedures", `
spec_version: operator-role-v0
gate_procedures:
  extra_gate:
    - a per-repo procedure step
`},
		{"mission", "mission", `
spec_version: operator-role-v0
mission: a repo-local mission statement
`},
		{"escalation", "escalation", `
spec_version: operator-role-v0
escalation:
  page_format: terse
`},
		{"forbidden", "forbidden", `
spec_version: operator-role-v0
forbidden:
  - a repo-local prohibition
`},
		// ADR-097 D1: the orchestration remit is base-spec procedure. An
		// overlay can neither override it nor remove it.
		{"orchestration", "orchestration", `
spec_version: operator-role-v0
orchestration:
  remit:
    - a repo-local remit step
  delegation:
    counterfactual_battery:
      when: a repo-local trigger
      read: a repo-local reading
`},
		{"orchestration_null", "orchestration", `
spec_version: operator-role-v0
orchestration: null
`},
	}
	for _, tc := range cases {
		field, overlay := tc.field, tc.overlay
		t.Run(tc.name, func(t *testing.T) {
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

// TestCampaignActorIdentity pins the campaign auto-driver's (E25.6)
// in-process audit identity: a stable subject that classifies as the
// role instance acting (audit.ActorAgent via IsTokenSubject), the
// closed write-scope set the gate-action handlers enforce, and the
// fresh-slice guarantee so a caller cannot mutate the shared set.
func TestCampaignActorIdentity(t *testing.T) {
	if CampaignActorSubject != "operator-agent/campaign" {
		t.Fatalf("CampaignActorSubject = %q, want %q", CampaignActorSubject, "operator-agent/campaign")
	}
	// Subject must carry the operator-agent prefix so it is classified as
	// the role instance acting (ActorAgent), not a human user.
	if !IsTokenSubject(CampaignActorSubject) {
		t.Errorf("IsTokenSubject(%q) = false, want true (campaign actions must stamp ActorAgent)", CampaignActorSubject)
	}

	scopes := CampaignActorScopes()
	want := map[string]bool{
		"write:approvals": false,
		"write:stages":    false,
		"write:fixups":    false,
		"write:retries":   false,
	}
	for _, s := range scopes {
		if _, ok := want[s]; !ok {
			t.Errorf("CampaignActorScopes() has unexpected scope %q", s)
			continue
		}
		want[s] = true
	}
	for s, seen := range want {
		if !seen {
			t.Errorf("CampaignActorScopes() missing required scope %q", s)
		}
	}

	// Fresh slice on each call: mutating one return must not affect the next.
	scopes[0] = "mutated"
	if again := CampaignActorScopes(); again[0] != "write:approvals" {
		t.Errorf("CampaignActorScopes() returned a shared backing array: got %q after mutation", again[0])
	}
}

func TestDelegatedApprovalActorSubject(t *testing.T) {
	if DelegatedApprovalActorSubject != "operator-agent/delegated" {
		t.Fatalf("DelegatedApprovalActorSubject = %q, want %q", DelegatedApprovalActorSubject, "operator-agent/delegated")
	}
	// It must carry the operator-agent prefix so a delegated approval recorded
	// under it is classified ActorAgent (the #1709 recorded-but-never-counted
	// vote), just like the campaign subject.
	if !IsTokenSubject(DelegatedApprovalActorSubject) {
		t.Errorf("IsTokenSubject(%q) = false, want true (a delegated approval must stamp ActorAgent)", DelegatedApprovalActorSubject)
	}
	// It is ATTRIBUTION only, never an issuable token subject: "delegated" is not
	// a recognized role-spec version, so token issuance must still refuse it.
	if err := ValidateTokenSubject(DelegatedApprovalActorSubject); err == nil {
		t.Errorf("ValidateTokenSubject(%q) = nil, want a refusal (it is not an issuable token subject)", DelegatedApprovalActorSubject)
	}
	// It must be DISTINCT from the campaign subject: a delegated approval and a
	// campaign auto-drive action are different identities and must not collide.
	if DelegatedApprovalActorSubject == CampaignActorSubject {
		t.Errorf("DelegatedApprovalActorSubject == CampaignActorSubject (%q); the two attribution identities must be distinct", DelegatedApprovalActorSubject)
	}
}

func TestDelegatedApprovalDecisionStates(t *testing.T) {
	// The two decision-state strings are the entire operator handoff of the
	// delegated-approval path; both sides (server emitter + local driver)
	// reference these symbols, so their VALUES are pinned here.
	if DecisionStateHumanQuorumRequired != "human_quorum_required" {
		t.Errorf("DecisionStateHumanQuorumRequired = %q, want %q", DecisionStateHumanQuorumRequired, "human_quorum_required")
	}
	if DecisionStateDelegatedApprovalNoProgress != "delegated_approval_no_progress" {
		t.Errorf("DecisionStateDelegatedApprovalNoProgress = %q, want %q", DecisionStateDelegatedApprovalNoProgress, "delegated_approval_no_progress")
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

// TestValidateTokenSubject covers the ADR-040 D4 issuance-time subject
// convention: prefixed subjects must name a recognized role-spec
// version; everything else passes untouched.
func TestValidateTokenSubject(t *testing.T) {
	cases := []struct {
		name    string
		subject string
		wantErr string // substring; "" means valid
	}{
		{"valid versioned subject", "operator-agent/operator-role-v0", ""},
		{"bare prefix without version", "operator-agent/", "followed by a role-spec version"},
		{"bare name without slash", "operator-agent", "followed by a role-spec version"},
		{"unknown version", "operator-agent/operator-role-v9", `unrecognized role-spec version "operator-role-v9"`},
		{"github subject unaffected", "github:42", ""},
		{"bootstrap subject unaffected", "bootstrap", ""},
		{"mcp subject unaffected", "mcp:run:00000000-0000-0000-0000-000000000001", ""},
		{"prefix-adjacent name unaffected", "operator-agents/x", ""},
		{"empty subject unaffected", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTokenSubject(tc.subject)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateTokenSubject(%q) = %v, want nil", tc.subject, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateTokenSubject(%q) = nil, want error containing %q", tc.subject, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
			if !strings.Contains(err.Error(), "operator-role-v0") {
				t.Errorf("error %q does not name the recognized versions", err.Error())
			}
		})
	}
}

// TestIsTokenSubject pins the prefix-match classification read paths
// key actor attribution on.
func TestIsTokenSubject(t *testing.T) {
	if !IsTokenSubject("operator-agent/operator-role-v0") {
		t.Error("IsTokenSubject(operator-agent/operator-role-v0) = false, want true")
	}
	if !IsTokenSubject("operator-agent/") {
		t.Error("IsTokenSubject(operator-agent/) = false, want true (prefix match only)")
	}
	for _, s := range []string{"operator-agent", "github:42", "", "agent/operator-role-v0"} {
		if IsTokenSubject(s) {
			t.Errorf("IsTokenSubject(%q) = true, want false", s)
		}
	}
}
