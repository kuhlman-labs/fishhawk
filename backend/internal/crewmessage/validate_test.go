package crewmessage

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "valid", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

// mutateFixture reads a valid fixture into a generic map and applies fn, so a
// reject case differs from an accepted document in exactly ONE field. That
// isolation is what makes each case a counterfactual for its own control
// rather than a rejection for some unrelated reason.
func mutateFixture(t *testing.T, name string, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(readFixture(t, name), &m); err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	fn(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal fixture %s: %v", name, err)
	}
	return out
}

// TestValidate_AcceptsEveryValidFixture is the accept path: one valid fixture
// per member of the closed five-type set (the issue's first acceptance
// criterion), each Validated against the EMBEDDED copy here and against the
// CANONICAL copy by check-jsonschema in the local hygiene step — so a
// divergence between the two layers fails on one side rather than passing
// per-layer.
//
// Parse FIDELITY is asserted field by field, not merely by message type: the
// anchor (WHICH member and its value), every payload field, every evidence
// reference, response_required and deadline. A wrong json tag or a dropped
// field therefore fails a test instead of round-tripping as a zero value.
func TestValidate_AcceptsEveryValidFixture(t *testing.T) {
	cases := []struct {
		file string
		want Message
	}{
		{
			file: "consult.json",
			want: Message{
				SchemaVersion: SchemaVersion,
				Type:          TypeConsult,
				SenderRole:    RolePlanner,
				RecipientRole: RoleHistorian,
				Anchor:        Anchor{RunID: "6f1d2c3a-4b5e-4f70-8a91-2c3d4e5f6a7b"},
				Payload: Payload{
					Question:      "Has a closed type set for crew messages been settled before, and if so where?",
					WhatICanInfer: "ADR-081 names five types; I cannot tell whether an earlier decision narrowed or widened that set.",
					Context:       "Planning E77.1, the crew-message contract child.",
				},
				Evidence: []EvidenceReference{
					{Kind: EvidenceDecisionRecord, Ref: "ADR-081"},
					{Kind: EvidenceIssue, Ref: "kuhlman-labs/fishhawk#3727"},
				},
				ResponseRequired: true,
				Deadline:         "2026-09-29T17:30:00Z",
			},
		},
		{
			file: "finding.json",
			want: Message{
				SchemaVersion: SchemaVersion,
				Type:          TypeFinding,
				SenderRole:    RoleSecurity,
				RecipientRole: RoleCaptain,
				Anchor:        Anchor{IssueRef: "kuhlman-labs/fishhawk#3735"},
				Payload: Payload{
					Summary:  "The evidence reference shape carries no repo-path member, so a citation cannot express a scope path.",
					Detail:   "Noted while reading the contract; this is an observation, not a concern, and blocks no gate.",
					Severity: "low",
				},
				Evidence: []EvidenceReference{
					{Kind: EvidenceAuditEntry, Ref: "3f6a1b02-9c4d-4e8f-b012-7a8b9c0d1e2f"},
				},
				ResponseRequired: false,
			},
		},
		{
			file: "work_request.json",
			want: Message{
				SchemaVersion: SchemaVersion,
				Type:          TypeWorkRequest,
				SenderRole:    RoleArchitect,
				RecipientRole: RoleCaptain,
				Anchor:        Anchor{DecisionRecordID: "ADR-081"},
				Payload: Payload{
					Title:     "Ship the crew-message prompt envelope",
					Summary:   "ADR-081 rule 5 requires delivered messages to be rendered only by a prompt-package function inside a CREW MESSAGE envelope.",
					Rationale: "The contract child ships no render path, so the envelope needs its own item before any delivery surface lands.",
				},
				Evidence: []EvidenceReference{
					{Kind: EvidenceDecisionRecord, Ref: "ADR-081"},
				},
			},
		},
		{
			file: "notice.json",
			want: Message{
				SchemaVersion: SchemaVersion,
				Type:          TypeNotice,
				SenderRole:    RoleHistorian,
				RecipientRole: RolePlanner,
				Anchor:        Anchor{RunID: "b2c3d4e5-f607-4819-a2b3-c4d5e6f70819"},
				Payload: Payload{
					Summary: "The precedent query found one prior decision on this surface and no contradicting one.",
					Detail:  "Recorded for the plan's grounding; no answer is expected.",
				},
				Evidence: []EvidenceReference{
					{Kind: EvidenceRun, Ref: "b2c3d4e5-f607-4819-a2b3-c4d5e6f70819"},
					{Kind: EvidenceURL, Ref: "https://github.com/kuhlman-labs/fishhawk/issues/3727"},
				},
				ResponseRequired: false,
			},
		},
		{
			file: "escalation.json",
			want: Message{
				SchemaVersion: SchemaVersion,
				Type:          TypeEscalation,
				SenderRole:    RoleReviewer,
				RecipientRole: RoleCaptain,
				Anchor:        Anchor{IssueRef: "kuhlman-labs/fishhawk#3727"},
				Payload: Payload{
					Summary:            "The planner and I disagree on whether the crew-role enum should admit implementer.",
					RecommendedDefault: "Keep implementer in the enum and enforce invariant #8 in the Go validator only.",
					Tradeoffs:          "Keeping it makes the Go rule the sole gate in its fixture's path, so the control stays counterfactual; removing it enforces the rule twice but masks the Go rule, leaving it untested dead code.",
				},
				Evidence: []EvidenceReference{
					{Kind: EvidenceStage, Ref: "2b9fced2-ed45-4248-9c65-6762d201d327"},
				},
				ResponseRequired: true,
				Deadline:         "2026-09-30T12:00:00Z",
			},
		},
	}

	if len(cases) != 5 {
		t.Fatalf("the closed type set has five members; accept table covers %d", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data := readFixture(t, tc.file)
			if err := Validate(data); err != nil {
				t.Fatalf("Validate(%s) = %v, want nil", tc.file, err)
			}
			got, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse(%s) = %v, want nil", tc.file, err)
			}
			// Whole-value comparison: every field of Message, its Anchor, its
			// Payload and its Evidence slice at once. A dropped field or a
			// wrong json tag decodes as a zero value and fails here.
			if !reflect.DeepEqual(*got, tc.want) {
				t.Errorf("Parse(%s) fidelity mismatch\n got: %#v\nwant: %#v", tc.file, *got, tc.want)
			}
			// Spelled-out anchor assertion: WHICH member carries the value
			// matters, so a tag swap between run_id / issue_ref /
			// decision_record_id cannot pass by carrying the right string in
			// the wrong field.
			if got.Anchor != tc.want.Anchor {
				t.Errorf("Parse(%s) anchor = %#v, want %#v", tc.file, got.Anchor, tc.want.Anchor)
			}
			if !reflect.DeepEqual(got.Evidence, tc.want.Evidence) {
				t.Errorf("Parse(%s) evidence = %#v, want %#v", tc.file, got.Evidence, tc.want.Evidence)
			}
			if got.Payload != tc.want.Payload {
				t.Errorf("Parse(%s) payload = %#v, want %#v", tc.file, got.Payload, tc.want.Payload)
			}
			if got.ResponseRequired != tc.want.ResponseRequired {
				t.Errorf("Parse(%s) response_required = %v, want %v", tc.file, got.ResponseRequired, tc.want.ResponseRequired)
			}
			if got.Deadline != tc.want.Deadline {
				t.Errorf("Parse(%s) deadline = %q, want %q", tc.file, got.Deadline, tc.want.Deadline)
			}
		})
	}
}

// TestValidate_RejectsImplementRecipient is CONTROL 1, the issue's named
// counterfactual: the Go no-implement-recipient rule (invariant #8).
//
// The fixture is well-formed in EVERY other field and differs from the
// accepted notice only in recipient_role. Because the schema's crew-role enum
// deliberately INCLUDES implementer, this document passes the schema layer by
// construction, so checkSemantics is the ONLY gate in its path: neuter that
// rule and Validate returns nil and this test reddens on errors.Is.
func TestValidate_RejectsImplementRecipient(t *testing.T) {
	data := mutateFixture(t, "notice.json", func(m map[string]any) {
		m["recipient_role"] = string(RoleImplementer)
	})

	// Precondition, asserted rather than assumed: the SCHEMA accepts this
	// document. If it ever stops doing so the Go rule is masked and this test
	// is no longer a counterfactual for it.
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse mutated fixture: %v", err)
	}
	if err := compiledSchema.Validate(raw); err != nil {
		t.Fatalf("precondition failed: the schema must ACCEPT recipient_role: implementer so the Go rule is the sole gate, got %v", err)
	}

	err := Validate(data)
	if !errors.Is(err, ErrRecipientNotAddressable) {
		t.Fatalf("Validate = %v, want ErrRecipientNotAddressable", err)
	}
	if !strings.Contains(err.Error(), string(RoleImplementer)) {
		t.Errorf("error must name the offending role, got %q", err)
	}
	if _, perr := Parse(data); !errors.Is(perr, ErrRecipientNotAddressable) {
		t.Errorf("Parse = %v, want ErrRecipientNotAddressable", perr)
	}
}

// TestValidate_RejectsResponseRequiredOnNotice is CONTROL 7: response_required
// is legal only on the two synchronously-answered types (ADR-081 D1).
//
// The schema declares response_required as a plain boolean on EVERY type, so
// this document is schema-valid and the Go rule is the only gate. The
// precondition below asserts that rather than assuming it.
func TestValidate_RejectsResponseRequiredOnNotice(t *testing.T) {
	data := mutateFixture(t, "notice.json", func(m map[string]any) {
		m["response_required"] = true
	})

	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse mutated fixture: %v", err)
	}
	if err := compiledSchema.Validate(raw); err != nil {
		t.Fatalf("precondition failed: the schema must ACCEPT response_required on a notice so the Go rule is the sole gate, got %v", err)
	}

	err := Validate(data)
	if !errors.Is(err, ErrResponseNotAnswerable) {
		t.Fatalf("Validate = %v, want ErrResponseNotAnswerable", err)
	}
	if !strings.Contains(err.Error(), string(TypeNotice)) {
		t.Errorf("error must name the offending type, got %q", err)
	}
}

// TestValidate_ResponseRequiredLegalOnAnswerableTypes is the paired accept
// arm: a rule that rejected response_required on EVERY type would pass the
// reject case above, so the two answerable types are pinned too. consult.json
// and escalation.json both carry response_required: true, and the accept table
// already Validates them — this states the intent explicitly.
func TestValidate_ResponseRequiredLegalOnAnswerableTypes(t *testing.T) {
	for _, f := range []string{"consult.json", "escalation.json"} {
		msg, err := Parse(readFixture(t, f))
		if err != nil {
			t.Fatalf("Parse(%s) = %v, want nil", f, err)
		}
		if !msg.ResponseRequired {
			t.Fatalf("%s must carry response_required: true to pin the answerable arm", f)
		}
	}
}

// The schema-layer reject table. Each case mutates exactly ONE field of an
// otherwise-valid fixture, and each asserts the returned error NAMES the
// offending property or keyword — so a rejection for an unrelated reason
// cannot green the case.
func TestValidate_SchemaLayerRejections(t *testing.T) {
	cases := []struct {
		// name doubles as the control's identity in the PR record.
		name string
		// base is the valid fixture the case mutates.
		base string
		// mutate applies the single-field defect.
		mutate func(m map[string]any)
		// wantIn are substrings the error must carry, naming the offending
		// property or keyword.
		wantIn []string
	}{
		{
			// CONTROL 2: the crew-role enum (ADR-081 rule 3). Addresses are
			// roles, never model instances. Replace the enum with type:string
			// and this fixture validates, because nothing else constrains the
			// property.
			name:   "model id recipient",
			base:   "consult.json",
			mutate: func(m map[string]any) { m["recipient_role"] = "claude-opus-5" },
			wantIn: []string{"/recipient_role", "must be one of"},
		},
		{
			// CONTROL 2b: the same enum guards the SENDER too, so a model
			// instance cannot masquerade as a crew role in either direction.
			name:   "model id sender",
			base:   "consult.json",
			mutate: func(m map[string]any) { m["sender_role"] = "gpt-5-codex" },
			wantIn: []string{"/sender_role", "must be one of"},
		},
		{
			// CONTROL 3: root additionalProperties:false (ADR-081 rule 2). A
			// scope path list is the field this contract most needs to be
			// unable to express. Every other field is valid, so the
			// unknown-property rejection is the sole failure cause.
			name: "scope authority field",
			base: "finding.json",
			mutate: func(m map[string]any) {
				m["scope"] = []any{"backend/internal/server/runs.go"}
			},
			wantIn: []string{"scope"},
		},
		{
			// CONTROL 3b: the payload $defs close themselves rather than
			// relying on the root — Draft 2020-12 evaluates
			// additionalProperties against the properties declared in the SAME
			// schema object, so a $ref'd subschema contributes nothing to the
			// parent's allowed set.
			name: "authority field inside the payload",
			base: "finding.json",
			mutate: func(m map[string]any) {
				m["payload"].(map[string]any)["forbidden_paths"] = []any{".github/workflows/**"}
			},
			wantIn: []string{"forbidden_paths"},
		},
		{
			// CONTROL 4: the anchor oneOf. Each anchor value is INDIVIDUALLY
			// valid, so the oneOf is the only thing failing — delete it and
			// both optional properties validate.
			name: "two anchors",
			base: "consult.json",
			mutate: func(m map[string]any) {
				m["anchor"].(map[string]any)["issue_ref"] = "kuhlman-labs/fishhawk#3735"
			},
			wantIn: []string{"oneOf"},
		},
		{
			// CONTROL 4b: the same oneOf refuses ZERO anchors, the other side
			// of exactly-one.
			name:   "no anchor",
			base:   "consult.json",
			mutate: func(m map[string]any) { m["anchor"] = map[string]any{} },
			wantIn: []string{"oneOf"},
		},
		{
			// CONTROL 5: the escalation arm's required entry for
			// recommended_default. tradeoffs is left PRESENT, which proves the
			// payload is otherwise well-formed, so only the then-clause
			// rejects it.
			name: "escalation missing recommended_default",
			base: "escalation.json",
			mutate: func(m map[string]any) {
				delete(m["payload"].(map[string]any), "recommended_default")
			},
			wantIn: []string{"recommended_default"},
		},
		{
			// CONTROL 5b (maintainer condition 1): the SAME arm's required
			// entry for tradeoffs, with recommended_default left PRESENT.
			// Making tradeoffs optional turns this case red. Without it,
			// dropping tradeoffs from the arm's required list would be silent.
			name: "escalation missing tradeoffs",
			base: "escalation.json",
			mutate: func(m map[string]any) {
				delete(m["payload"].(map[string]any), "tradeoffs")
			},
			wantIn: []string{"tradeoffs"},
		},
		{
			// CONTROL 6: the type enum. With the enum deleted no if/then arm
			// matches, leaving payload unconstrained, so the document
			// validates and this case reddens.
			name:   "unknown type",
			base:   "notice.json",
			mutate: func(m map[string]any) { m["type"] = "order" },
			wantIn: []string{"/type", "must be one of", "'consult'"},
		},
		{
			// The evidence-reference kind enum is closed and deliberately
			// omits any file/path member: a repo path is the nearest thing to
			// a scope expression (ADR-081 rule 2).
			name: "file evidence kind",
			base: "finding.json",
			mutate: func(m map[string]any) {
				m["evidence"] = []any{map[string]any{"kind": "file", "ref": "backend/internal/server/runs.go"}}
			},
			wantIn: []string{"/evidence/0/kind", "must be one of"},
		},
		{
			// An evidence reference cannot smuggle a path in an extra member
			// either: evidence-reference closes itself.
			name: "path member on an evidence reference",
			base: "finding.json",
			mutate: func(m map[string]any) {
				m["evidence"] = []any{map[string]any{"kind": "issue", "ref": "kuhlman-labs/fishhawk#1", "path": "backend/main.go"}}
			},
			wantIn: []string{"path"},
		},
		{
			// The schema_version const: a document declaring another version
			// is not this contract.
			name:   "wrong schema version",
			base:   "notice.json",
			mutate: func(m map[string]any) { m["schema_version"] = "crew-message-v2" },
			wantIn: []string{"crew-message-v1"},
		},
		{
			// A per-type payload arm rejects a payload belonging to another
			// type, so consult prose cannot ride on a notice.
			name: "notice carrying a consult payload",
			base: "notice.json",
			mutate: func(m map[string]any) {
				m["payload"] = map[string]any{"question": "why?"}
			},
			wantIn: []string{"summary"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := mutateFixture(t, tc.base, tc.mutate)
			err := Validate(data)
			if err == nil {
				t.Fatalf("Validate = nil, want a schema rejection")
			}
			var serr *SchemaError
			if !errors.As(err, &serr) {
				t.Fatalf("Validate = %T (%v), want *SchemaError", err, err)
			}
			for _, want := range tc.wantIn {
				if !strings.Contains(serr.Error(), want) {
					t.Errorf("error must name %q so the rejection is for the RIGHT reason, got %q", want, serr)
				}
			}
		})
	}
}

// TestParse_RejectsMalformedAndEmpty pins the two ParseError branches.
func TestParse_RejectsMalformedAndEmpty(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"empty", ""},
		{"whitespace only", "   \n\t "},
		{"not json", "{"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.in))
			var perr *ParseError
			if !errors.As(err, &perr) {
				t.Fatalf("Parse = %T (%v), want *ParseError", err, err)
			}
		})
	}
}

// TestParse_DisallowsUnknownFields pins the typed decoder's own refusal. It is
// a SECOND gate behind the schema's additionalProperties:false, and it exists
// for drift the schema cannot see: a Message json tag that stops matching a
// schema property name would leave the field schema-valid but silently dropped
// on decode. The assertion is on the observable refusal, using a
// schema-invisible shape (a payload field spelled the way a renamed tag would
// leave it) so the branch is reachable.
func TestParse_DisallowsUnknownFields(t *testing.T) {
	// The schema layer is what refuses this document, so assert the layered
	// outcome: it never reaches the typed decode.
	data := mutateFixture(t, "notice.json", func(m map[string]any) {
		m["payload"].(map[string]any)["summary_text"] = "renamed tag"
	})
	if err := Validate(data); err == nil {
		t.Fatal("Validate = nil, want a rejection of the unknown payload field")
	}

	// And the decoder itself refuses an unknown field when handed one
	// directly, which is the guard that catches a tag/property drift the
	// schema would accept.
	var msg Message
	dec := json.NewDecoder(strings.NewReader(`{"schema_version":"crew-message-v1","not_a_field":1}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&msg); err == nil {
		t.Fatal("typed decode accepted an unknown field; Parse must decode with DisallowUnknownFields")
	}
}
