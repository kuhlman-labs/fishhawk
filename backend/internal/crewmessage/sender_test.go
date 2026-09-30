package crewmessage

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// noticeWithoutSender is the notice fixture with sender_role removed, in the
// fixture's own (indented) layout so a re-marshal is observable as a byte
// difference.
func noticeWithoutSender(t *testing.T) []byte {
	t.Helper()
	raw := readFixture(t, "notice.json")
	out := bytes.Replace(raw, []byte(`  "sender_role": "historian",`+"\n"), nil, 1)
	if bytes.Equal(out, raw) {
		t.Fatal("precondition failed: the notice fixture's sender_role line was not removed — the fixture layout changed")
	}
	return out
}

// TestWithSenderRole_InjectsWhenAbsent is the injection arm: an absent
// sender_role is set to the derived role, and the result is a document Parse
// accepts with every other field intact.
func TestWithSenderRole_InjectsWhenAbsent(t *testing.T) {
	in := noticeWithoutSender(t)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(in, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["sender_role"]; ok {
		t.Fatal("precondition failed: fixture still carries sender_role")
	}

	out, err := WithSenderRole(in, RolePlanner)
	if err != nil {
		t.Fatalf("WithSenderRole: %v", err)
	}
	msg, err := Parse(out)
	if err != nil {
		t.Fatalf("Parse(injected) = %v, want nil", err)
	}
	if msg.SenderRole != RolePlanner {
		t.Errorf("SenderRole = %q, want the injected %q", msg.SenderRole, RolePlanner)
	}
	want, err := Parse(readFixture(t, "notice.json"))
	if err != nil {
		t.Fatal(err)
	}
	want.SenderRole = RolePlanner
	gotJSON, _ := json.Marshal(msg)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("injection changed a field other than sender_role:\n got  %s\n want %s", gotJSON, wantJSON)
	}
}

// TestWithSenderRole_ReturnsRawBytesUnchangedWhenMatching pins the no-rewrite
// arm: a document already naming the derived role is returned BYTE-IDENTICAL.
// The fixture is indented, so any re-marshal (which compacts and reorders)
// would be observable.
func TestWithSenderRole_ReturnsRawBytesUnchangedWhenMatching(t *testing.T) {
	in := readFixture(t, "notice.json") // sender_role: historian
	out, err := WithSenderRole(in, RoleHistorian)
	if err != nil {
		t.Fatalf("WithSenderRole: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("a matching sender_role must return the caller's bytes unchanged\n got  %s\n want %s", out, in)
	}
}

// TestWithSenderRole_RefusesMismatchedSenderRole is the sender-role control.
// Every fixture is well-formed in every other field; the reviewer case names a
// SCHEMA-VALID role, so the comparison is the only gate in its path (asserted
// as a precondition). Presence is decided on the raw member, so null, an empty
// string and a non-string value are present-and-different, never absent.
func TestWithSenderRole_RefusesMismatchedSenderRole(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"schema-valid different role", "reviewer"},
		{"implementer claimed by a planner", "implementer"},
		{"not a crew role", "gpt-5-codex"},
		{"empty string", ""},
		{"explicit null", nil},
		{"non-string", 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := mutateFixture(t, "notice.json", func(m map[string]any) { m["sender_role"] = tc.value })
			if tc.value == "reviewer" {
				if _, err := Parse(in); err != nil {
					t.Fatalf("precondition failed: the document must be valid on its own so the comparison is the sole gate, got %v", err)
				}
			}
			out, err := WithSenderRole(in, RolePlanner)
			if !errors.Is(err, ErrSenderRoleNotSettable) {
				t.Fatalf("WithSenderRole = (%s, %v), want ErrSenderRoleNotSettable", out, err)
			}
			if out != nil {
				t.Errorf("a refusal must return no bytes, got %s", out)
			}
		})
	}
}

// TestWithSenderRole_RejectsDuplicateMembersBeforeInjection pins the ORDER:
// the duplicate-member walk runs over the caller's raw bytes BEFORE the
// injection's map re-marshal can collapse a duplicate last-wins. The fixtures
// carry NO sender_role, so the injection path is the one taken, and each
// asserts as a precondition that the COLLAPSED document (with the role
// injected) is fully valid — so the walk is the sole gate. Moving the walk
// after the re-marshal launders the duplicate: the helper returns the
// collapsed bytes and Parse accepts them with one anchor silently chosen, the
// escape the package README records as observed.
func TestWithSenderRole_RejectsDuplicateMembersBeforeInjection(t *testing.T) {
	const head = `{"schema_version":"crew-message-v1","type":"notice","recipient_role":"planner",`
	const anchorRun = `"anchor":{"run_id":"b2c3d4e5-f607-4819-a2b3-c4d5e6f70819"}`
	const tail = `"payload":{"summary":"s","detail":"d"}}`
	cases := []struct {
		name    string
		in      string
		wantDup string
	}{
		{"repeated anchor with different kinds", head + anchorRun + `,"anchor":{"issue_ref":"kuhlman-labs/fishhawk#1"},` + tail, "anchor"},
		{"repeated recipient_role", head + anchorRun + `,"recipient_role":"security",` + tail, "recipient_role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Precondition: collapse exactly as the injection path would, then
			// require the collapsed document to Parse cleanly.
			var collapsed map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.in), &collapsed); err != nil {
				t.Fatalf("precondition failed: fixture must be well-formed JSON: %v", err)
			}
			if _, ok := collapsed["sender_role"]; ok {
				t.Fatal("precondition failed: fixture must omit sender_role so the injection path is taken")
			}
			collapsed["sender_role"] = json.RawMessage(`"planner"`)
			cb, _ := json.Marshal(collapsed)
			if _, err := Parse(cb); err != nil {
				t.Fatalf("precondition failed: the collapsed document must be valid so the walk is the sole gate, got %v", err)
			}

			out, err := WithSenderRole([]byte(tc.in), RolePlanner)
			if !errors.Is(err, ErrDuplicateMember) {
				t.Fatalf("WithSenderRole = (%s, %v), want ErrDuplicateMember", out, err)
			}
			if !strings.Contains(err.Error(), tc.wantDup) {
				t.Errorf("error must name the repeated member %q, got %q", tc.wantDup, err)
			}
		})
	}

	// A duplicate NESTED below the root is not laundered by the injection
	// (top-level values are carried as json.RawMessage, so nested bytes
	// survive and Parse would still refuse it), but the walk refuses it here
	// first, at admission.
	t.Run("repeated member inside the anchor", func(t *testing.T) {
		in := head + `"anchor":{"run_id":"b2c3d4e5-f607-4819-a2b3-c4d5e6f70819","run_id":"0f0e0d0c-0b0a-4908-8706-050403020100"},` + tail
		if _, err := WithSenderRole([]byte(in), RolePlanner); !errors.Is(err, ErrDuplicateMember) {
			t.Fatalf("WithSenderRole = %v, want ErrDuplicateMember", err)
		}
	})

	// A duplicated sender_role is refused as a duplicate, not compared: with a
	// last-wins decode the second value matches and the raw bytes would pass.
	t.Run("repeated sender_role", func(t *testing.T) {
		in := head + `"sender_role":"reviewer","sender_role":"planner",` + anchorRun + `,` + tail
		if _, err := WithSenderRole([]byte(in), RolePlanner); !errors.Is(err, ErrDuplicateMember) {
			t.Fatalf("WithSenderRole = %v, want ErrDuplicateMember", err)
		}
	})
}

// TestWithSenderRole_RefusesUnknownRole pins the role-vocabulary guard: a
// server-derived role outside AllRoles is a programming error and must not be
// injected into a document Parse would then reject for a misleading reason.
func TestWithSenderRole_RefusesUnknownRole(t *testing.T) {
	out, err := WithSenderRole(noticeWithoutSender(t), Role("engineer"))
	if !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("WithSenderRole = (%s, %v), want ErrUnknownRole", out, err)
	}
	// Every member of the vocabulary IS accepted — the implementer is a legal
	// sender (only addressing one is forbidden).
	for _, r := range AllRoles {
		if _, err := WithSenderRole(noticeWithoutSender(t), r); err != nil {
			t.Errorf("WithSenderRole(%q) = %v, want nil", r, err)
		}
	}
}

// TestWithSenderRole_RejectsNonObjectDocuments covers the parse-error branches.
// A top-level null decodes into a NIL map without error, so it has its own arm:
// without it the injection would write into a nil map and panic.
func TestWithSenderRole_RejectsNonObjectDocuments(t *testing.T) {
	for _, in := range []string{"", "   \n", "null", "[]", `"x"`, "{", `{"a":1} trailing`} {
		t.Run(in, func(t *testing.T) {
			out, err := WithSenderRole([]byte(in), RolePlanner)
			var perr *ParseError
			if !errors.As(err, &perr) {
				t.Fatalf("WithSenderRole(%q) = (%s, %v), want *ParseError", in, out, err)
			}
		})
	}
}
