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
			// CONTROL 8: format assertion on anchor.run_id. Draft 2020-12
			// makes `format` an ANNOTATION by default, so without
			// Compiler.AssertFormat the Go layer accepts this document while
			// check-jsonschema — which asserts formats — rejects it against
			// the canonical copy. Every other field is valid, so the format
			// keyword is the sole failure cause; drop AssertFormat and this
			// case reddens.
			name: "malformed run_id uuid",
			base: "consult.json",
			mutate: func(m map[string]any) {
				m["anchor"].(map[string]any)["run_id"] = "not-a-uuid"
			},
			wantIn: []string{"/anchor/run_id", "uuid"},
		},
		{
			// CONTROL 8b: the same assertion on deadline's date-time. The
			// reference doc advertises 'RFC 3339 date-time', so a document
			// carrying prose in the field must be refused by the layer that
			// advertises it.
			name:   "malformed deadline date-time",
			base:   "consult.json",
			mutate: func(m map[string]any) { m["deadline"] = "not-a-date" },
			wantIn: []string{"/deadline", "date-time"},
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

// TestDecodeStrict_RejectsUnknownField pins the PRODUCTION unknown-field
// guard: decodeStrict is the function Parse calls, and the sole site of
// dec.DisallowUnknownFields(). Delete that call and this test reddens.
//
// The earlier shape of this test could not do that. No document can both pass
// the schema (every object is additionalProperties:false) and carry a field
// Message does not know, so a test driving Parse cannot reach the branch at
// all; the old test's first arm was therefore a SCHEMA rejection and its
// second arm built its own decoder, exercising encoding/json rather than the
// production wiring. Both stayed green with the guard removed. Testing the
// extracted production function is what makes the guard counterfactual.
func TestDecodeStrict_RejectsUnknownField(t *testing.T) {
	// A field spelled the way a renamed json tag would leave it: the drift
	// this guard exists for.
	in := []byte(`{"schema_version":"crew-message-v1","type":"notice","sender_role":"historian","recipient_role":"planner","anchor":{"run_id":"b2c3d4e5-f607-4819-a2b3-c4d5e6f70819"},"payload":{"summary_text":"renamed tag"}}`)

	// Precondition, asserted rather than assumed: the document is WELL-FORMED
	// JSON and decodes fine WITHOUT the guard, so the rejection below is the
	// guard firing and not a malformed-input artifact.
	var permissive Message
	if err := json.Unmarshal(in, &permissive); err != nil {
		t.Fatalf("precondition failed: the fixture must decode without the guard, got %v", err)
	}

	_, err := decodeStrict(in)
	var perr *ParseError
	if !errors.As(err, &perr) {
		t.Fatalf("decodeStrict = %T (%v), want *ParseError from DisallowUnknownFields", err, err)
	}
	if !strings.Contains(perr.Error(), "summary_text") {
		t.Errorf("error must name the unknown field, got %q", perr)
	}

	// The guard must not fire on a legal document: a rule that rejected
	// everything would pass the arm above.
	if _, err := decodeStrict(readFixture(t, "notice.json")); err != nil {
		t.Fatalf("decodeStrict(notice.json) = %v, want nil", err)
	}
}

// TestParse_RoutesThroughDecodeStrict pins the WIRING: Parse must return the
// decodeStrict value, not an independently decoded one. Asserted through a
// legal document, since no schema-valid document reaches the guard branch.
func TestParse_RoutesThroughDecodeStrict(t *testing.T) {
	data := readFixture(t, "consult.json")
	viaParse, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	viaHelper, err := decodeStrict(data)
	if err != nil {
		t.Fatalf("decodeStrict = %v, want nil", err)
	}
	if !reflect.DeepEqual(viaParse, viaHelper) {
		t.Errorf("Parse must return the decodeStrict value\n got: %#v\nwant: %#v", viaParse, viaHelper)
	}
}

// TestParse_RejectsDuplicateMembers is the duplicate-member control.
//
// The two decoders inside Parse DISAGREE on a repeated member: the generic
// decode REPLACES (map assignment), the typed decode MERGES (a struct field
// that is itself a struct is decoded into twice). The anchor case below is
// the consequence that matters — before this control, the schema layer saw
// ONE anchor and passed the oneOf while the decoded Message carried TWO,
// letting exactly-one-anchor escape into the typed value.
//
// These fixtures are RAW BYTES, not mutateFixture output: a map[string]any
// cannot hold a duplicate member, so the map-based helper structurally cannot
// reach this boundary.
func TestParse_RejectsDuplicateMembers(t *testing.T) {
	const anchorRun = `"anchor":{"run_id":"b2c3d4e5-f607-4819-a2b3-c4d5e6f70819"}`
	const noticeTail = `"payload":{"summary":"s","detail":"d"}`
	const noticeHead = `"schema_version":"crew-message-v1","type":"notice","sender_role":"historian","recipient_role":"planner"`

	cases := []struct {
		name    string
		in      string
		wantDup string
	}{
		{
			// The consequential case: two anchors of DIFFERENT kinds. Each is
			// individually valid and the generic decode keeps only the last,
			// so the schema oneOf passes — the Go layer is the only gate.
			name:    "repeated anchor with different kinds",
			in:      `{` + noticeHead + `,` + anchorRun + `,"anchor":{"issue_ref":"kuhlman-labs/fishhawk#1"},` + noticeTail + `}`,
			wantDup: "anchor",
		},
		{
			// A repeated scalar at the root. Both values are legal members of
			// the crew-role enum, so the COLLAPSED document is schema-valid
			// either way and the rejection cannot come from the enum.
			name:    "repeated scalar member",
			in:      `{` + noticeHead + `,` + anchorRun + `,` + noticeTail + `,"sender_role":"security"}`,
			wantDup: "sender_role",
		},
		{
			// A repeated member NESTED inside the anchor object, proving the
			// walk descends rather than checking the root only.
			name:    "repeated member inside the anchor",
			in:      `{` + noticeHead + `,"anchor":{"run_id":"b2c3d4e5-f607-4819-a2b3-c4d5e6f70819","run_id":"0f0e0d0c-0b0a-4908-8706-050403020100"},` + noticeTail + `}`,
			wantDup: "run_id",
		},
		{
			// A repeated member inside an ARRAY element, proving the walk
			// descends through arrays too.
			name:    "repeated member inside an evidence element",
			in:      `{` + noticeHead + `,` + anchorRun + `,` + noticeTail + `,"evidence":[{"kind":"run","ref":"a","ref":"b"}]}`,
			wantDup: "ref",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Precondition, asserted rather than assumed: with the duplicate
			// COLLAPSED the way the generic decoder collapses it, the document
			// is schema-valid. So the rejection below is this control firing,
			// not the schema rejecting something else about the fixture.
			var raw any
			if err := json.Unmarshal([]byte(tc.in), &raw); err != nil {
				t.Fatalf("precondition failed: fixture must be well-formed JSON, got %v", err)
			}
			if err := compiledSchema.Validate(raw); err != nil {
				t.Fatalf("precondition failed: the schema must ACCEPT the collapsed document so the Go rule is the sole gate, got %v", err)
			}

			_, err := Parse([]byte(tc.in))
			if !errors.Is(err, ErrDuplicateMember) {
				t.Fatalf("Parse = %v, want ErrDuplicateMember", err)
			}
			if !strings.Contains(err.Error(), tc.wantDup) {
				t.Errorf("error must name the repeated member %q, got %q", tc.wantDup, err)
			}
			if verr := Validate([]byte(tc.in)); !errors.Is(verr, ErrDuplicateMember) {
				t.Errorf("Validate = %v, want ErrDuplicateMember", verr)
			}
		})
	}
}

// TestParse_AcceptsRepeatedNameInSiblingObjects is the paired accept arm: a
// rule that rejected any name seen twice ANYWHERE in the document would pass
// the reject table above. The same member name in two SIBLING objects is
// legal and must stay accepted — every evidence element carries "kind" and
// "ref".
func TestParse_AcceptsRepeatedNameInSiblingObjects(t *testing.T) {
	for _, f := range []string{"consult.json", "notice.json"} {
		msg, err := Parse(readFixture(t, f))
		if err != nil {
			t.Fatalf("Parse(%s) = %v, want nil", f, err)
		}
		if len(msg.Evidence) < 2 {
			t.Fatalf("%s must carry two evidence elements so the sibling-name arm is exercised, got %d", f, len(msg.Evidence))
		}
	}
}
