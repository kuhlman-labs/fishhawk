package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// recordEscapeStub answers POST /v0/runs/{run_id}/escapes with status/body,
// recording each request line and body.
type recordEscapeStub struct {
	requests []string
	bodies   []string
	status   int
	body     string
}

func (b *recordEscapeStub) resolver(t *testing.T) *runResolver {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		b.bodies = append(b.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return newResolver(ts, nil)
}

func decodeSentKeys(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		t.Fatalf("decode sent body %q: %v", raw, err)
	}
	return keys
}

// TestRecordEscape_RecordForwardsBody: a RECORD POSTs the run-scoped route
// with the fields verbatim (delegated included), carries no CLOSE key, and
// returns the backend's 201 body.
func TestRecordEscape_RecordForwardsBody(t *testing.T) {
	runID := uuid.New()
	stub := &recordEscapeStub{status: http.StatusCreated, body: `{"run_id":"` + runID.String() +
		`","audit_sequence":42,"record_kind":"birth","state":"pending_judgement","judgement_basis":"awaiting_attestation",` +
		`"pending_reason":"agent_recorded","source_event_key":"idempotency_key:bug-1"}`}
	r := stub.resolver(t)
	_, out, err := r.recordEscape(context.Background(), nil, recordEscapeInput{
		RunID: runID.String(), Source: "human_finding", Severity: "high", Category: "correctness",
		Description: "nil deref on empty list", Files: []string{"backend/x.go"}, IdempotencyKey: "bug-1", Delegated: true,
	})
	if err != nil {
		t.Fatalf("recordEscape: %v", err)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "POST /v0/runs/"+runID.String()+"/escapes" {
		t.Fatalf("requests = %v, want one POST to the escapes route", stub.requests)
	}
	keys := decodeSentKeys(t, stub.bodies[0])
	if string(keys["delegated"]) != "true" {
		t.Errorf("delegated = %s, want true forwarded", keys["delegated"])
	}
	for _, absent := range []string{"supersedes_sequence", "attestation", "security_class", "advisory_id", "concern_id", "head_sha", "introducing_commits"} {
		if _, ok := keys[absent]; ok {
			t.Errorf("RECORD body carries %q: %s", absent, stub.bodies[0])
		}
	}
	var sent recordEscapeRecordBody
	_ = json.Unmarshal([]byte(stub.bodies[0]), &sent)
	if sent.Source != "human_finding" || sent.Description != "nil deref on empty list" || len(sent.Files) != 1 || sent.IdempotencyKey != "bug-1" {
		t.Errorf("sent = %+v, want the record fields verbatim", sent)
	}
	res := out.Result
	if res.AuditSequence != 42 || res.RecordKind != "birth" || res.State != "pending_judgement" || res.PendingReason != "agent_recorded" || res.SourceEventKey != "idempotency_key:bug-1" {
		t.Errorf("result = %+v, want the backend's 201 body", res)
	}
}

// TestRecordEscape_SecurityRecordOmitsFreeText: a security RECORD never
// forwards a PRESENT-but-empty description or files key (the backend refuses
// either as free text).
func TestRecordEscape_SecurityRecordOmitsFreeText(t *testing.T) {
	stub := &recordEscapeStub{status: http.StatusCreated, body: `{"record_kind":"birth"}`}
	r := stub.resolver(t)
	if _, _, err := r.recordEscape(context.Background(), nil, recordEscapeInput{
		RunID: uuid.NewString(), Source: "operator_battery", Severity: "high", Category: "security",
		IdempotencyKey: uuid.NewString(), SecurityClass: "authz_bypass", AdvisoryID: "GHSA-jfh8-c2jp-5v3q",
	}); err != nil {
		t.Fatalf("recordEscape: %v", err)
	}
	keys := decodeSentKeys(t, stub.bodies[0])
	for _, absent := range []string{"description", "files", "introducing_commits", "security_finding_id"} {
		if _, ok := keys[absent]; ok {
			t.Errorf("security RECORD body carries %q: %s", absent, stub.bodies[0])
		}
	}
	if string(keys["security_class"]) != `"authz_bypass"` || string(keys["advisory_id"]) != `"GHSA-jfh8-c2jp-5v3q"` {
		t.Errorf("security body = %s, want class and advisory id forwarded", stub.bodies[0])
	}
}

// TestRecordEscape_CloseForwardsBody: a CLOSE sends ONLY supersedes_sequence,
// attestation and delegated, and returns the closure.
func TestRecordEscape_CloseForwardsBody(t *testing.T) {
	runID := uuid.New()
	stub := &recordEscapeStub{status: http.StatusCreated, body: `{"run_id":"` + runID.String() +
		`","audit_sequence":43,"record_kind":"closure","state":"counted","judgement_basis":"operator_attestation","supersedes_sequence":"42"}`}
	r := stub.resolver(t)
	_, out, err := r.recordEscape(context.Background(), nil, recordEscapeInput{
		RunID: runID.String(), SupersedesSequence: 42, Attestation: &recordEscapeAttestation{Judgement: "defect", Note: "confirmed"},
	})
	if err != nil {
		t.Fatalf("recordEscape: %v", err)
	}
	keys := decodeSentKeys(t, stub.bodies[0])
	if len(keys) != 3 {
		t.Errorf("CLOSE body keys = %v, want exactly supersedes_sequence, attestation, delegated", keys)
	}
	if string(keys["supersedes_sequence"]) != "42" || string(keys["delegated"]) != "false" {
		t.Errorf("CLOSE body = %s", stub.bodies[0])
	}
	var att recordEscapeAttestation
	if err := json.Unmarshal(keys["attestation"], &att); err != nil || att.Judgement != "defect" || att.Note != "confirmed" {
		t.Errorf("attestation = %+v (%v), want defect / confirmed", att, err)
	}
	if out.Result.RecordKind != "closure" || out.Result.SupersedesSequence != "42" || out.Result.State != "counted" {
		t.Errorf("result = %+v, want the closure", out.Result)
	}
}

// TestRecordEscape_LocalValidation: a malformed call fails before the HTTP
// hop — the backend sees zero requests.
func TestRecordEscape_LocalValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   recordEscapeInput
		want string
	}{
		{"bad run id", recordEscapeInput{RunID: "nope", Source: "human_finding"}, "run_id"},
		{"close without attestation", recordEscapeInput{RunID: uuid.NewString(), SupersedesSequence: 7}, "requires attestation"},
		{"close mixing record fields", recordEscapeInput{RunID: uuid.NewString(), SupersedesSequence: 7,
			Attestation: &recordEscapeAttestation{Judgement: "defect"}, Category: "security"}, "drop the RECORD fields"},
		{"attestation without supersedes", recordEscapeInput{RunID: uuid.NewString(), Source: "human_finding",
			Attestation: &recordEscapeAttestation{Judgement: "defect"}}, "set supersedes_sequence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &recordEscapeStub{body: `{}`}
			r := stub.resolver(t)
			_, _, err := r.recordEscape(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(stub.requests) != 0 {
				t.Errorf("backend requests = %v, want 0 (local validation short-circuits)", stub.requests)
			}
		})
	}
}

// TestRecordEscape_BackendRefusalsTyped: each backend refusal surfaces as a
// tool error wrapping the typed *apiError with its status and code.
func TestRecordEscape_BackendRefusalsTyped(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusForbidden, "run_token_forbidden"},
		{http.StatusForbidden, "escape_attestation_requires_forge_credential"},
		{http.StatusConflict, "escape_already_recorded"},
		{http.StatusUnprocessableEntity, "escape_security_free_text_refused"},
		{http.StatusBadRequest, "validation_failed"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			stub := &recordEscapeStub{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
			r := stub.resolver(t)
			_, _, err := r.recordEscape(context.Background(), nil, recordEscapeInput{
				RunID: uuid.NewString(), Source: "human_finding", Severity: "low", Category: "other",
				Description: "d", Files: []string{"a.go"}, IdempotencyKey: "k",
			})
			var ae *apiError
			if !errors.As(err, &ae) || ae.StatusCode != tc.status || ae.Code != tc.code {
				t.Fatalf("err = %v (%#v), want *apiError %d %s", err, ae, tc.status, tc.code)
			}
		})
	}
}
