package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// crewStub answers the /v0/crew-messages surface with canned bodies,
// recording each request line and body.
type crewStub struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	status   int
	body     string
}

func (b *crewStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		b.bodies = append(b.bodies, string(raw))
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

const crewRecordBody = `{"sent_sequence":7,"thread_root_sequence":7,"message_type":"escalation","sender_role":"captain","recipient_role":"planner","anchor":{"run_id":"11111111-1111-1111-1111-111111111111"},"response_required":false,"state":"open","round":0,"sent_at":"2026-09-01T00:00:00Z"}`

// crewEnvelope renders text the way prompt.RenderCrewMessages frames it —
// enough for the bound to measure; the MCP layer never builds one itself.
func crewEnvelope(text string) string {
	return "<<<BEGIN UNTRUSTED CREW MESSAGE>>>\n" + text + "\n<<<END UNTRUSTED CREW MESSAGE>>>\n"
}

func crewGetBody(rendered, answer string) string {
	v := map[string]any{}
	_ = json.Unmarshal([]byte(crewRecordBody), &v)
	v["rendered"] = rendered
	if answer != "" {
		v["answered"] = true
		v["answer"] = map[string]any{"sent_sequence": 8, "sender_role": "planner", "rendered": answer}
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func crewListBody(n int, anchorPad int) string {
	var items []string
	for i := 1; i <= n; i++ {
		items = append(items, fmt.Sprintf(`{"sent_sequence":%d,"thread_root_sequence":%d,"message_type":"notice","sender_role":"captain","recipient_role":"planner","anchor":{"issue_ref":"kuhlman-labs/fishhawk#%s"},"response_required":false,"state":"open","round":0,"sent_at":"2026-09-01T00:00:00Z"}`,
			i, i, strings.Repeat("9", anchorPad)))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// TestSendCrewMessage_PostsDocumentAndDecodes: send POSTs the caller's
// document verbatim to /v0/crew-messages and returns the recorded row.
func TestSendCrewMessage_PostsDocumentAndDecodes(t *testing.T) {
	b := &crewStub{status: http.StatusCreated, body: crewRecordBody}
	r := digestResolver(b.serve(t).URL, nil)
	doc := map[string]any{"schema_version": "1", "type": "escalation", "recipient_role": "planner",
		"anchor": map[string]any{"run_id": "11111111-1111-1111-1111-111111111111"}, "payload": map[string]any{"text": "hi"}}
	_, out, err := r.sendCrewMessage(context.Background(), nil, SendCrewMessageInput{Message: doc})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(b.requests) != 1 || b.requests[0] != "POST /v0/crew-messages" {
		t.Errorf("requests = %v, want one POST /v0/crew-messages", b.requests)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(b.bodies[0]), &sent); err != nil || sent["type"] != "escalation" {
		t.Errorf("body = %s, want the caller's document", b.bodies[0])
	}
	if _, has := sent["sender_role"]; has {
		t.Errorf("body = %s: the tool must not inject a sender_role — the backend derives it", b.bodies[0])
	}
	if out.Message.SentSequence != 7 || out.Message.SenderRole != "captain" || out.Elisions != nil {
		t.Errorf("out = %+v, want the decoded row, unbounded", out)
	}
}

// TestCrewMessageRecord_ToleratesOlderBackend: a body missing every optional
// field decodes (pointer DTO fields), so an older backend cannot break it.
func TestCrewMessageRecord_ToleratesOlderBackend(t *testing.T) {
	b := &crewStub{status: http.StatusCreated, body: `{"sent_sequence":3,"message_type":"notice","sender_role":"captain","recipient_role":"planner","anchor":{"issue_ref":"o/r#1"},"state":"open"}`}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.sendCrewMessage(context.Background(), nil, SendCrewMessageInput{Message: map[string]any{"type": "notice"}})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	m := out.Message
	if m.SentSequence != 3 || m.Round != nil || m.SentAt != nil || m.ProjectionDegraded != nil || m.Anchor.IssueRef == nil {
		t.Errorf("record = %+v, want absent optional fields as nil", m)
	}
}

// TestCrewMessageTools_LocalRefusals: each local validation refuses BEFORE
// any request is made.
func TestCrewMessageTools_LocalRefusals(t *testing.T) {
	b := &crewStub{body: crewRecordBody}
	r := digestResolver(b.serve(t).URL, nil)
	ctx := context.Background()
	if _, _, err := r.sendCrewMessage(ctx, nil, SendCrewMessageInput{}); err == nil || !strings.Contains(err.Error(), "message is required") {
		t.Errorf("send without message: err = %v, want message is required", err)
	}
	for _, in := range []DecideCrewEscalationInput{
		{Decision: "accepted"},
		{Sequence: 7, Decision: "maybe"},
		{Sequence: 7},
	} {
		if _, _, err := r.decideCrewEscalation(ctx, nil, in); err == nil {
			t.Errorf("decide(%+v) = nil error, want a refusal", in)
		}
	}
	for name, in := range map[string]ReadCrewMessagesInput{
		"negative sequence":     {Sequence: -1},
		"sequence with filter":  {Sequence: 7, RunID: "11111111-1111-1111-1111-111111111111"},
		"sequence with limit":   {Sequence: 7, Limit: 5},
		"wait without sequence": {Wait: 5, RecipientRole: "captain"},
	} {
		if _, _, err := r.readCrewMessages(ctx, nil, in); err == nil {
			t.Errorf("%s: read = nil error, want a refusal", name)
		}
	}
	if len(b.requests) != 0 {
		t.Errorf("requests = %v, want none (refused locally)", b.requests)
	}
}

// TestDecideCrewEscalation_PostsDecision: the decision POSTs the sequence's
// escalation-decision route with {decision, reason}.
func TestDecideCrewEscalation_PostsDecision(t *testing.T) {
	b := &crewStub{body: strings.Replace(crewRecordBody, `"state":"open"`, `"state":"accepted"`, 1)}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.decideCrewEscalation(context.Background(), nil, DecideCrewEscalationInput{Sequence: 7, Decision: " accepted ", Reason: "ok"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if b.requests[0] != "POST /v0/crew-messages/7/escalation-decision" || b.bodies[0] != `{"decision":"accepted","reason":"ok"}` {
		t.Errorf("request = %s %s", b.requests[0], b.bodies[0])
	}
	if out.Message.State != "accepted" {
		t.Errorf("state = %q, want accepted", out.Message.State)
	}
}

// TestReadCrewMessages_RoutesByInput: sequence reads one (with ?wait=
// clamped to 30), filters list with the query the backend expects.
func TestReadCrewMessages_RoutesByInput(t *testing.T) {
	ctx := context.Background()
	b := &crewStub{body: crewGetBody(crewEnvelope("hello"), "")}
	r := digestResolver(b.serve(t).URL, nil)
	for _, in := range []ReadCrewMessagesInput{{Sequence: 7}, {Sequence: 7, Wait: 300}, {Sequence: 7, Wait: -4}} {
		_, out, err := r.readCrewMessages(ctx, nil, in)
		if err != nil {
			t.Fatalf("read %+v: %v", in, err)
		}
		if out.Message == nil || out.Message.Rendered != crewEnvelope("hello") || out.Messages != nil {
			t.Errorf("out = %+v, want the one message with its rendered envelope", out)
		}
	}
	want := []string{"GET /v0/crew-messages/7", "GET /v0/crew-messages/7?wait=30", "GET /v0/crew-messages/7"}
	for i, w := range want {
		if b.requests[i] != w {
			t.Errorf("request %d = %s, want %s", i, b.requests[i], w)
		}
	}

	lb := &crewStub{body: crewListBody(2, 1)}
	lr := digestResolver(lb.serve(t).URL, nil)
	for _, in := range []ReadCrewMessagesInput{
		{RecipientRole: "captain", State: "open", Limit: 10},
		{RunID: "11111111-1111-1111-1111-111111111111"},
		{},
	} {
		_, out, err := lr.readCrewMessages(ctx, nil, in)
		if err != nil {
			t.Fatalf("list %+v: %v", in, err)
		}
		if len(out.Messages) != 2 || out.Message != nil || out.Elisions != nil {
			t.Errorf("list out = %+v, want two messages, unbounded", out)
		}
	}
	wantList := []string{
		"GET /v0/crew-messages?limit=10&recipient_role=captain&state=open",
		"GET /v0/crew-messages?run_id=11111111-1111-1111-1111-111111111111",
		"GET /v0/crew-messages",
	}
	for i, w := range wantList {
		if lb.requests[i] != w {
			t.Errorf("list request %d = %s, want %s", i, lb.requests[i], w)
		}
	}
}

// TestCrewMessageTools_RefusalPassthrough: backend refusals surface as the
// typed *apiError with status and code intact, on every tool.
func TestCrewMessageTools_RefusalPassthrough(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "sender_role_not_settable"},
		{http.StatusForbidden, "insufficient_scope"},
		{http.StatusNotFound, "crew_message_not_found"},
		{http.StatusConflict, "crew_message_already_disposed"},
		{http.StatusServiceUnavailable, "crew_message_unconfigured"},
	} {
		b := &crewStub{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
		r := digestResolver(b.serve(t).URL, nil)
		calls := map[string]func() error{
			"send": func() error {
				_, _, err := r.sendCrewMessage(ctx, nil, SendCrewMessageInput{Message: map[string]any{"type": "notice"}})
				return err
			},
			"read": func() error {
				_, _, err := r.readCrewMessages(ctx, nil, ReadCrewMessagesInput{Sequence: 7})
				return err
			},
			"list": func() error { _, _, err := r.readCrewMessages(ctx, nil, ReadCrewMessagesInput{}); return err },
			"decide": func() error {
				_, _, err := r.decideCrewEscalation(ctx, nil, DecideCrewEscalationInput{Sequence: 7, Decision: "rejected"})
				return err
			},
		}
		for name, call := range calls {
			var ae *apiError
			if err := call(); !errors.As(err, &ae) || ae.StatusCode != tc.status || ae.Code != tc.code {
				t.Errorf("%s: err = %v, want *apiError %d %s", name, err, tc.status, tc.code)
			}
		}
	}
}

// TestReadCrewMessages_BoundedUnderBudget: an under-budget response is
// byte-identical to an unbounded marshal of the decoded body, with NO
// elisions block.
func TestReadCrewMessages_BoundedUnderBudget(t *testing.T) {
	body := crewGetBody(crewEnvelope(strings.Repeat("r", 2000)), crewEnvelope("answer"))
	b := &crewStub{body: body}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.readCrewMessages(context.Background(), nil, ReadCrewMessagesInput{Sequence: 7})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var want CrewMessageView
	if err := json.Unmarshal([]byte(body), &want); err != nil {
		t.Fatal(err)
	}
	gotRaw, _ := json.Marshal(out)
	wantRaw, _ := json.Marshal(ReadCrewMessagesOutput{Message: &want})
	if string(gotRaw) != string(wantRaw) {
		t.Errorf("under-budget wire differs from the unbounded marshal:\n got %s\nwant %s", gotRaw, wantRaw)
	}
	if out.Elisions != nil {
		t.Errorf("elisions = %+v, want none under budget", out.Elisions)
	}
}

// TestReadCrewMessages_TiersDownOverBudget: B1 caps rendered envelopes and
// fits; each result is MEASURED within the budget with the elisions attached.
func TestReadCrewMessages_TiersDownOverBudget(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	body := crewGetBody(crewEnvelope(strings.Repeat("r", 9000)), crewEnvelope(strings.Repeat("a", 9000)))
	b := &crewStub{body: body}
	r := digestResolver(b.serve(t).URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)})
	_, out, err := r.readCrewMessages(context.Background(), nil, ReadCrewMessagesInput{Sequence: 7})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > budget {
		t.Errorf("output is %d bytes, want <= %d", len(raw), budget)
	}
	if out.Elisions == nil || out.Elisions.Tier != "B1" || len(out.Elisions.Fields) != 2 {
		t.Fatalf("elisions = %+v, want tier B1 with two rendered-field entries", out.Elisions)
	}
	for _, f := range out.Elisions.Fields {
		if !strings.HasPrefix(f.Pointer, "GET /v0/crew-messages/7") {
			t.Errorf("elision %s pointer = %q, want the unbounded REST surface", f.Field, f.Pointer)
		}
	}
	if !strings.HasPrefix(out.Message.Rendered, "<<<BEGIN UNTRUSTED CREW MESSAGE>>>") || out.Message.SentSequence != 7 {
		t.Errorf("message = %+v, want capped envelope and intact metadata", out.Message)
	}
}

// TestReadCrewMessages_ListDropsOldest: a listing over budget drops the
// OLDEST messages (the backend lists ascending), keeps the newest, and fits.
func TestReadCrewMessages_ListDropsOldest(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	b := &crewStub{body: crewListBody(40, 1)}
	r := digestResolver(b.serve(t).URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)})
	_, out, err := r.readCrewMessages(context.Background(), nil, ReadCrewMessagesInput{RecipientRole: "planner"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > budget {
		t.Errorf("output is %d bytes, want <= %d", len(raw), budget)
	}
	if out.Elisions == nil || out.Elisions.Tier != "B2" || len(out.Elisions.Fields) != 1 || out.Elisions.Fields[0].Field != "messages" {
		t.Fatalf("elisions = %+v, want one B2 messages entry", out.Elisions)
	}
	m := out.Messages
	if len(m) == 0 || m[len(m)-1].SentSequence != 40 || out.Elisions.Fields[0].OmittedCount != 40-len(m) {
		t.Errorf("kept %d ending at %v, omitted %d — want the NEWEST kept and the count honest", len(m), m, out.Elisions.Fields[0].OmittedCount)
	}
	if out.Elisions.Fields[0].Pointer != "GET /v0/crew-messages?recipient_role=planner" {
		t.Errorf("pointer = %q, want the unbounded listing", out.Elisions.Fields[0].Pointer)
	}
}

// TestReadCrewMessages_FloorWhenOneDoesNotFit: a single listed message too
// large for the budget forces the constant-size floor.
func TestReadCrewMessages_FloorWhenOneDoesNotFit(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	for name, body := range map[string]string{
		"listing": crewListBody(1, 9000),
		"one": strings.Replace(crewGetBody(crewEnvelope("x"), ""), `"run_id":"11111111-1111-1111-1111-111111111111"`,
			`"issue_ref":"`+strings.Repeat("i", 9000)+`"`, 1),
	} {
		b := &crewStub{body: body}
		r := digestResolver(b.serve(t).URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)})
		in := ReadCrewMessagesInput{RecipientRole: "planner"}
		if name == "one" {
			in = ReadCrewMessagesInput{Sequence: 7}
		}
		_, out, err := r.readCrewMessages(context.Background(), nil, in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, _ := json.Marshal(out)
		if len(raw) > budget {
			t.Errorf("%s: floor output is %d bytes, want <= %d", name, len(raw), budget)
		}
		if out.Elisions == nil || out.Elisions.Tier != floorTierName {
			t.Errorf("%s: elisions = %+v, want the floor tier", name, out.Elisions)
		}
		if name == "one" && (out.Message == nil || out.Message.Rendered != "" || out.Message.SentSequence != 7) {
			t.Errorf("one: message = %+v, want metadata kept and the envelope dropped", out.Message)
		}
		if name == "listing" && (out.Messages == nil || len(out.Messages) != 0) {
			t.Errorf("listing: messages = %v, want an empty listing", out.Messages)
		}
	}
}

// TestCrewRecordOutput_FloorOverBudget: send/decide results over budget are
// capped to the floor; under budget they are unchanged.
func TestCrewRecordOutput_FloorOverBudget(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	body := strings.Replace(crewRecordBody, `"run_id":"11111111-1111-1111-1111-111111111111"`, `"decision_record_id":"`+strings.Repeat("d", 9000)+`"`, 1)
	b := &crewStub{body: body}
	r := digestResolver(b.serve(t).URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)})
	_, out, err := r.decideCrewEscalation(context.Background(), nil, DecideCrewEscalationInput{Sequence: 7, Decision: "accepted"})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > budget || out.Elisions == nil || out.Elisions.Tier != floorTierName || out.Message.SentSequence != 7 {
		t.Errorf("out (%d bytes) = %+v, want the floor within %d bytes", len(raw), out.Elisions, budget)
	}
}
