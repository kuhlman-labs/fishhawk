package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The no-database crew-message refusal suite (E77.3 / #3737). Every refusal
// here fires BEFORE the mailbox is reached: the mailbox is
// crewmessage.NewMailbox(nil, 0), so a refusal that failed to fire would
// reach pgx with a nil pool and panic rather than pass. The pgtest-backed
// cross-layer suite is crewmessage_pg_test.go.

// crewDoc renders a crew-message-v1 document. anchor is the raw JSON of the
// anchor object; senderRole "" omits the member (the server derives it).
func crewDoc(msgType, senderRole, recipient, anchor, payload string) string {
	var b strings.Builder
	b.WriteString(`{"schema_version":"crew-message-v1","type":"` + msgType + `"`)
	if senderRole != "" {
		b.WriteString(`,"sender_role":"` + senderRole + `"`)
	}
	b.WriteString(`,"recipient_role":"` + recipient + `","anchor":` + anchor + `,"payload":` + payload + `}`)
	return b.String()
}

func runAnchor(id uuid.UUID) string { return `{"run_id":"` + id.String() + `"}` }

const crewConsultPayload = `{"question":"Has this been decided before?"}`

// crewNoDBServer wires a server whose mailbox has NO pool, over a fake run
// repo holding two runs: one whose executing stage is plan and one whose
// executing stage is implement.
func crewNoDBServer(t *testing.T) (s *Server, planRun, implRun *run.Run) {
	t.Helper()
	rr := newOrchestratorRepo()
	planRun = rr.seedRun()
	rr.seedStage(planRun.ID, 1, run.StageStateRunning).Type = run.StageTypePlan
	implRun = rr.seedRun()
	rr.seedStage(implRun.ID, 1, run.StageStateRunning).Type = run.StageTypeImplement
	s = New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     rr,
		AuditRepo:   &auditCapture{},
		CrewMailbox: crewmessage.NewMailbox(nil, 0),
	})
	return s, planRun, implRun
}

// crewCall drives one handler directly with an injected identity.
func crewCall(t *testing.T, h http.HandlerFunc, method, target, sequence, body string, decorate func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if sequence != "" {
		req.SetPathValue("sequence", sequence)
	}
	if decorate != nil {
		req = decorate(req)
	}
	w := httptest.NewRecorder()
	h(w, req)
	return w
}

// requireCrewRefusal asserts the status AND the error code by identity.
func requireCrewRefusal(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, status, w.Body.String())
	}
	if got := errorCode(t, w); got != code {
		t.Fatalf("error code = %q, want %q; body = %s", got, code, w.Body.String())
	}
}

func runBound(runID uuid.UUID, scopes ...string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request { return withRunBoundIdentity(r, runID, scopes...) }
}

func operator(scopes ...string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request { return withOperatorIdentity(r, scopes...) }
}

// implementTokenScopes is exactly what handleIssueMCPToken mints for an
// implement stage (no workflow spec): no write:messages.
var implementTokenScopes = []string{"mcp:read", "write:scope-amendments"}

// (12) nil mailbox -> 503 on every mailbox-backed route.
func TestCrewMessageAPI_Unconfigured503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: newOrchestratorRepo(), AuditRepo: &auditCapture{}})
	op := operator("write:stages", "read:audit")
	for name, w := range map[string]*httptest.ResponseRecorder{
		"send":     crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", "{}", op),
		"get":      crewCall(t, s.handleGetCrewMessage, http.MethodGet, "/v0/crew-messages/1", "1", "", op),
		"list":     crewCall(t, s.handleListCrewMessages, http.MethodGet, "/v0/crew-messages", "", "", op),
		"decision": crewCall(t, s.handleDecideCrewEscalation, http.MethodPost, "/v0/crew-messages/1/escalation-decision", "1", "{}", op),
	} {
		t.Run(name, func(t *testing.T) {
			requireCrewRefusal(t, w, http.StatusServiceUnavailable, "crew_message_unconfigured")
		})
	}
}

func TestCrewMessageAPI_Anonymous401(t *testing.T) {
	s, _, _ := crewNoDBServer(t)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"send":     crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", "{}", nil),
		"get":      crewCall(t, s.handleGetCrewMessage, http.MethodGet, "/v0/crew-messages/1", "1", "", nil),
		"list":     crewCall(t, s.handleListCrewMessages, http.MethodGet, "/v0/crew-messages", "", "", nil),
		"respond":  crewCall(t, s.handleRespondCrewMessage, http.MethodPost, "/v0/crew-messages/1/respond", "1", "{}", nil),
		"decision": crewCall(t, s.handleDecideCrewEscalation, http.MethodPost, "/v0/crew-messages/1/escalation-decision", "1", "{}", nil),
	} {
		t.Run(name, func(t *testing.T) { requireCrewRefusal(t, w, http.StatusUnauthorized, "authentication_required") })
	}
}

// (1) an implement-stage run token (no write:messages) cannot send.
func TestCrewMessageAPI_SendImplementTokenInsufficientScope(t *testing.T) {
	s, _, implRun := crewNoDBServer(t)
	body := crewDoc("consult", "", "historian", runAnchor(implRun.ID), crewConsultPayload)
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
		runBound(implRun.ID, implementTokenScopes...))
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
}

// A token that holds write:messages but whose run's executing stage is now
// implement derives no sender role (the token outlived its stage).
func TestCrewMessageAPI_SendStageNotDerivable(t *testing.T) {
	s, _, implRun := crewNoDBServer(t)
	body := crewDoc("consult", "", "historian", runAnchor(implRun.ID), crewConsultPayload)
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
		runBound(implRun.ID, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusForbidden, "crew_sender_not_derivable")
}

// (2) cross-run anchor and a run-LESS anchor are both refused for a run token.
func TestCrewMessageAPI_SendCrossRun403(t *testing.T) {
	s, planRun, implRun := crewNoDBServer(t)
	tok := runBound(planRun.ID, "mcp:read", scopeWriteMessages)
	t.Run("other_run", func(t *testing.T) {
		body := crewDoc("consult", "", "historian", runAnchor(implRun.ID), crewConsultPayload)
		w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body, tok)
		requireCrewRefusal(t, w, http.StatusForbidden, "cross_run_crew_message")
	})
	t.Run("run_less", func(t *testing.T) {
		body := crewDoc("consult", "", "historian", `{"issue_ref":"kuhlman-labs/fishhawk#3737"}`, crewConsultPayload)
		w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body, tok)
		requireCrewRefusal(t, w, http.StatusForbidden, "cross_run_crew_message")
	})
}

// (3) a body naming a sender role other than the derived one is refused.
func TestCrewMessageAPI_SendSenderRoleNotSettable(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := crewDoc("consult", "engineer", "historian", runAnchor(planRun.ID), crewConsultPayload)
	// "engineer" is not a crew role at all; "reviewer" is a real one that is
	// still not the planner's. Both are refused by the same rule.
	for _, b := range []string{body, crewDoc("consult", "reviewer", "historian", runAnchor(planRun.ID), crewConsultPayload)} {
		w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", b,
			runBound(planRun.ID, "mcp:read", scopeWriteMessages))
		requireCrewRefusal(t, w, http.StatusBadRequest, "sender_role_not_settable")
	}
}

// (10) implementer is SCHEMA-valid, so the Go addressability rule is the gate.
func TestCrewMessageAPI_SendRecipientNotAddressable(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := crewDoc("consult", "", "implementer", runAnchor(planRun.ID), crewConsultPayload)
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
		runBound(planRun.ID, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "recipient_not_addressable")
}

// (11) a document repeating a member name.
func TestCrewMessageAPI_SendDuplicateMember400(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := `{"schema_version":"crew-message-v1","type":"consult","recipient_role":"historian",` +
		`"anchor":` + runAnchor(planRun.ID) + `,"anchor":{"issue_ref":"x/y#1"},"payload":` + crewConsultPayload + `}`
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
		runBound(planRun.ID, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
}

// A schema violation names the failing instance location.
func TestCrewMessageAPI_SendSchemaViolation400(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := crewDoc("consult", "", "historian", runAnchor(planRun.ID), `{"question":""}`)
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
		runBound(planRun.ID, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
}

// An operator identity sends as captain only with write:stages.
func TestCrewMessageAPI_SendOperatorWithoutWriteStages403(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := crewDoc("notice", "", "planner", runAnchor(planRun.ID), `{"summary":"s"}`)
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body, operator("read:audit"))
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
}

// Condition 1: an implement-stage token is refused 403 on every read route
// BEFORE any read. (The pg suite pins the same refusal against a real message
// the token's own run holds, which is the counterfactual vehicle.)
func TestCrewMessageAPI_ReadsRefuseImplementToken(t *testing.T) {
	s, _, implRun := crewNoDBServer(t)
	tok := runBound(implRun.ID, implementTokenScopes...)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"get":      crewCall(t, s.handleGetCrewMessage, http.MethodGet, "/v0/crew-messages/1", "1", "", tok),
		"get_wait": crewCall(t, s.handleGetCrewMessage, http.MethodGet, "/v0/crew-messages/1?wait=5", "1", "", tok),
		"list":     crewCall(t, s.handleListCrewMessages, http.MethodGet, "/v0/crew-messages?run_id="+implRun.ID.String(), "", "", tok),
	} {
		t.Run(name, func(t *testing.T) { requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope") })
	}
}

// A run token listing another run's anchor, or listing by recipient (which
// spans every anchor), is refused.
func TestCrewMessageAPI_ListRunBoundBinding(t *testing.T) {
	s, planRun, implRun := crewNoDBServer(t)
	tok := runBound(planRun.ID, "mcp:read", scopeWriteMessages)
	t.Run("other_run", func(t *testing.T) {
		w := crewCall(t, s.handleListCrewMessages, http.MethodGet, "/v0/crew-messages?run_id="+implRun.ID.String(), "", "", tok)
		requireCrewRefusal(t, w, http.StatusForbidden, "cross_run_crew_message")
	})
	t.Run("run_less", func(t *testing.T) {
		w := crewCall(t, s.handleListCrewMessages, http.MethodGet, "/v0/crew-messages?issue_ref=x/y%231", "", "", tok)
		requireCrewRefusal(t, w, http.StatusForbidden, "cross_run_crew_message")
	})
	t.Run("recipient", func(t *testing.T) {
		w := crewCall(t, s.handleListCrewMessages, http.MethodGet, "/v0/crew-messages?recipient_role=planner", "", "", tok)
		requireCrewRefusal(t, w, http.StatusForbidden, "cross_run_crew_message")
	})
}

// (13) naming both filter families is refused; so is a malformed anchor/limit.
func TestCrewMessageAPI_ListValidation400(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	op := operator("read:audit")
	for name, target := range map[string]string{
		"both_families":  "/v0/crew-messages?recipient_role=planner&run_id=" + planRun.ID.String(),
		"two_anchors":    "/v0/crew-messages?run_id=" + planRun.ID.String() + "&issue_ref=x/y%231",
		"bad_run_id":     "/v0/crew-messages?run_id=nope",
		"bad_limit":      "/v0/crew-messages?limit=0",
		"limit_over_cap": "/v0/crew-messages?limit=100000",
	} {
		t.Run(name, func(t *testing.T) {
			requireCrewRefusal(t, crewCall(t, s.handleListCrewMessages, http.MethodGet, target, "", "", op),
				http.StatusBadRequest, "validation_failed")
		})
	}
}

func TestCrewMessageAPI_ListOperatorWithoutReadAudit403(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	w := crewCall(t, s.handleListCrewMessages, http.MethodGet, "/v0/crew-messages?run_id="+planRun.ID.String(), "", "", operator("write:stages"))
	requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
}

func TestCrewMessageAPI_GetBadSequence400(t *testing.T) {
	s, _, _ := crewNoDBServer(t)
	w := crewCall(t, s.handleGetCrewMessage, http.MethodGet, "/v0/crew-messages/x", "x", "", operator("read:audit"))
	requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
}

// (4) + (5) the respond refusal ladder. Each arm presents a token that WOULD
// satisfy another crew endpoint, so the refusal is attributable to its rung.
func TestCrewMessageAPI_RespondRefusalLadder(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	t.Run("run_bound", func(t *testing.T) {
		w := crewCall(t, s.handleRespondCrewMessage, http.MethodPost, "/v0/crew-messages/1/respond", "1", "{}",
			runBound(planRun.ID, "mcp:read", scopeWriteMessages))
		requireCrewRefusal(t, w, http.StatusForbidden, "agent_token_required")
	})
	t.Run("operator", func(t *testing.T) {
		w := crewCall(t, s.handleRespondCrewMessage, http.MethodPost, "/v0/crew-messages/1/respond", "1", "{}",
			operator("write:stages", "read:audit"))
		requireCrewRefusal(t, w, http.StatusForbidden, "responder_required")
	})
}

// (6) + (7) the escalation-decision gates.
func TestCrewMessageAPI_EscalationDecisionGates(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := `{"decision":"accepted","reason":"ok"}`
	t.Run("self_decision", func(t *testing.T) {
		w := crewCall(t, s.handleDecideCrewEscalation, http.MethodPost, "/v0/crew-messages/1/escalation-decision", "1", body,
			runBound(planRun.ID, "mcp:read", scopeWriteMessages, "write:stages"))
		requireCrewRefusal(t, w, http.StatusForbidden, "self_decision")
	})
	t.Run("insufficient_scope", func(t *testing.T) {
		w := crewCall(t, s.handleDecideCrewEscalation, http.MethodPost, "/v0/crew-messages/1/escalation-decision", "1", body,
			operator("read:audit", "write:runs", "write:approvals"))
		requireCrewRefusal(t, w, http.StatusForbidden, "insufficient_scope")
	})
	for name, b := range map[string]string{
		"expired_not_a_decision": `{"decision":"expired"}`,
		"unknown_field":          `{"decision":"accepted","extra":1}`,
		"not_json":               `nope`,
	} {
		t.Run(name, func(t *testing.T) {
			w := crewCall(t, s.handleDecideCrewEscalation, http.MethodPost, "/v0/crew-messages/1/escalation-decision", "1", b,
				operator("write:stages"))
			requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
		})
	}
}

func TestParseCrewMessageWaitSeconds(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 0}, {"abc", 0}, {"1.5", 0}, {"0", 0}, {"-3", 0},
		{"1", 1}, {"29", 29}, {"30", maxCrewMessageWaitSeconds},
		{"300", maxCrewMessageWaitSeconds}, // an order of magnitude over the cap
	} {
		if got := parseCrewMessageWaitSeconds(tc.raw); got != tc.want {
			t.Errorf("parseCrewMessageWaitSeconds(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

func TestCrewSenderRoleForStage(t *testing.T) {
	for _, tc := range []struct {
		st   run.StageType
		want crewmessage.Role
		ok   bool
	}{
		{run.StageTypePlan, crewmessage.RolePlanner, true},
		{run.StageTypeReview, crewmessage.RoleReviewer, true},
		{run.StageTypeImplement, "", false},
		{run.StageTypeDeploy, "", false},
		{run.StageTypeAcceptance, "", false},
		{"", "", false},
	} {
		got, ok := crewSenderRoleForStage(tc.st)
		if got != tc.want || ok != tc.ok {
			t.Errorf("crewSenderRoleForStage(%q) = (%q, %v), want (%q, %v)", tc.st, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCrewAccountVisible(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name     string
		id, row  *uuid.UUID
		expected bool
	}{
		{"no_identity_account", nil, &a, true},
		{"untenanted_row", &a, nil, true},
		{"same", &a, &a, true},
		{"other", &a, &b, false},
	} {
		if got := crewAccountVisible(tc.id, tc.row); got != tc.expected {
			t.Errorf("%s: crewAccountVisible = %v, want %v", tc.name, got, tc.expected)
		}
	}
}

// crewMessageForPrompt puts every payload field into the quarantined text
// and nothing untrusted into the metadata.
func TestCrewMessageForPrompt(t *testing.T) {
	msg, err := crewmessage.Parse([]byte(crewDoc("consult", "planner", "historian",
		`{"issue_ref":"x/y#1"}`, `{"question":"Q?","context":"C"}`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cm := crewMessageForPrompt(msg)
	if cm.MessageText != "question: Q?\ncontext: C" {
		t.Errorf("MessageText = %q", cm.MessageText)
	}
	if cm.AnchorRef != "issue_ref x/y#1" || cm.SenderRole != "planner" || cm.Type != "consult" {
		t.Errorf("metadata = %+v", cm)
	}
	raw, _ := json.Marshal(crewRowToResponse(crewmessage.Row{}))
	if strings.Contains(string(raw), "payload") || strings.Contains(string(raw), "message_text") {
		t.Errorf("row response carries a text field: %s", raw)
	}
}

// writeCrewMessageError maps every crewmessage sentinel onto its named
// refusal, and anything unrecognised onto a 500.
func TestWriteCrewMessageError_Mapping(t *testing.T) {
	s, _, _ := crewNoDBServer(t)
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"parse", &crewmessage.ParseError{Msg: "bad json"}, http.StatusBadRequest, "validation_failed"},
		{"not_found", fmt.Errorf("wrapped: %w", crewmessage.ErrMessageNotFound), http.StatusNotFound, "crew_message_not_found"},
		{"already_disposed", crewmessage.ErrAlreadyDisposed, http.StatusConflict, "crew_message_already_disposed"},
		{"invalid_disposition", crewmessage.ErrInvalidDisposition, http.StatusBadRequest, "validation_failed"},
		{"round_bound", crewmessage.ErrRoundBoundExhausted, http.StatusUnprocessableEntity, "crew_round_bound_exhausted"},
		{"thread_root_not_found", crewmessage.ErrThreadRootNotFound, http.StatusUnprocessableEntity, "crew_thread_invalid"},
		{"thread_root_not_root", crewmessage.ErrThreadRootNotRoot, http.StatusUnprocessableEntity, "crew_thread_invalid"},
		{"thread_cross_account", crewmessage.ErrThreadCrossAccount, http.StatusUnprocessableEntity, "crew_thread_invalid"},
		{"thread_anchor_mismatch", crewmessage.ErrThreadAnchorMismatch, http.StatusUnprocessableEntity, "crew_thread_invalid"},
		{"unknown", errors.New("pool exploded"), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.writeCrewMessageError(w, httptest.NewRequest(http.MethodGet, "/v0/crew-messages/1", nil), tc.err)
			requireCrewRefusal(t, w, tc.status, tc.code)
		})
	}
}

// errBodyReader fails every Read, so readCrewBody takes its non-size branch.
type errBodyReader struct{}

func (errBodyReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// A send or decision body over the cap is 413; an unreadable one is 400.
func TestCrewMessageAPI_BodyReadFailures(t *testing.T) {
	s, _, _ := crewNoDBServer(t)
	oversize := strings.Repeat("x", crewMessageMaxBodyBytes+1)
	t.Run("send_too_large", func(t *testing.T) {
		w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", oversize, operator("write:stages"))
		requireCrewRefusal(t, w, http.StatusRequestEntityTooLarge, "body_too_large")
	})
	t.Run("decision_too_large", func(t *testing.T) {
		w := crewCall(t, s.handleDecideCrewEscalation, http.MethodPost, "/v0/crew-messages/1/escalation-decision", "1", oversize, operator("write:stages"))
		requireCrewRefusal(t, w, http.StatusRequestEntityTooLarge, "body_too_large")
	})
	t.Run("send_unreadable", func(t *testing.T) {
		req := withOperatorIdentity(httptest.NewRequest(http.MethodPost, "/v0/crew-messages", errBodyReader{}), "write:stages")
		w := httptest.NewRecorder()
		s.handleSendCrewMessage(w, req)
		requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
	})
}

func TestCrewMessageAPI_DecisionBadSequence400(t *testing.T) {
	s, _, _ := crewNoDBServer(t)
	w := crewCall(t, s.handleDecideCrewEscalation, http.MethodPost, "/v0/crew-messages/0/escalation-decision", "0",
		`{"decision":"accepted"}`, operator("write:stages"))
	requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
}

// A run lookup that misses is 404 run_not_found and one that fails is 500,
// for both the token's own run and an operator's anchor run — each BEFORE the
// (pool-less) mailbox is reached.
func TestCrewMessageAPI_SendRunLookupFailures(t *testing.T) {
	s, _, _ := crewNoDBServer(t)
	ghost := uuid.New()
	t.Run("token_run_missing", func(t *testing.T) {
		body := crewDoc("consult", "", "historian", runAnchor(ghost), crewConsultPayload)
		w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
			runBound(ghost, "mcp:read", scopeWriteMessages))
		requireCrewRefusal(t, w, http.StatusNotFound, "run_not_found")
	})
	t.Run("operator_anchor_run_missing", func(t *testing.T) {
		body := crewDoc("notice", "", "planner", runAnchor(ghost), `{"summary":"s"}`)
		w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body, operator("write:stages"))
		requireCrewRefusal(t, w, http.StatusNotFound, "run_not_found")
	})

	broken := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     &getRunErrRepo{newOrchestratorRepo()},
		AuditRepo:   &auditCapture{},
		CrewMailbox: crewmessage.NewMailbox(nil, 0),
	})
	t.Run("token_run_error", func(t *testing.T) {
		body := crewDoc("consult", "", "historian", runAnchor(ghost), crewConsultPayload)
		w := crewCall(t, broken.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
			runBound(ghost, "mcp:read", scopeWriteMessages))
		requireCrewRefusal(t, w, http.StatusInternalServerError, "internal_error")
	})
	t.Run("operator_anchor_run_error", func(t *testing.T) {
		body := crewDoc("notice", "", "planner", runAnchor(ghost), `{"summary":"s"}`)
		w := crewCall(t, broken.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body, operator("write:stages"))
		requireCrewRefusal(t, w, http.StatusInternalServerError, "internal_error")
	})
}

func TestRespondToCrewMessage_Unconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	reply, answered, err := s.RespondToCrewMessage(context.Background(), RespondParams{SentSequence: 1})
	if err == nil || reply != nil || answered != nil {
		t.Fatalf("RespondToCrewMessage on a nil mailbox = (%v, %v, %v), want an error and no rows", reply, answered, err)
	}
}

// The decision-record anchor and evidence refs reach the prompt shape as
// metadata; evidence is rendered kind:ref.
func TestCrewMessageForPrompt_DecisionRecordAnchorAndEvidence(t *testing.T) {
	doc := `{"schema_version":"crew-message-v1","type":"consult","sender_role":"planner","recipient_role":"historian",` +
		`"anchor":{"decision_record_id":"dr-1"},"payload":{"question":"Q?"},` +
		`"evidence":[{"kind":"issue","ref":"x/y#1"},{"kind":"audit_entry","ref":"42"}]}`
	msg, err := crewmessage.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cm := crewMessageForPrompt(msg)
	if cm.AnchorRef != "decision_record_id dr-1" {
		t.Errorf("AnchorRef = %q", cm.AnchorRef)
	}
	if strings.Join(cm.EvidenceRefs, ",") != "issue:x/y#1,audit_entry:42" {
		t.Errorf("EvidenceRefs = %v", cm.EvidenceRefs)
	}
}

// E77.5 / #3739: a response_required consult under the empty (production)
// responder registry is refused crew_responder_unavailable BEFORE the mailbox
// is reached — this server's mailbox has no pool, so reaching Send would fail
// with a different error.
func TestCrewMessageAPI_ConsultRefusedBeforeMailboxWithoutResponder(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := `{"schema_version":"crew-message-v1","type":"consult","recipient_role":"historian","anchor":` +
		runAnchor(planRun.ID) + `,"payload":` + crewConsultPayload + `,"response_required":true}`
	w := crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
		runBound(planRun.ID, "mcp:read", scopeWriteMessages))
	requireCrewRefusal(t, w, http.StatusUnprocessableEntity, "crew_responder_unavailable")
}

// --- E77.6 (#3740): the send-time escalation notify condition ---------------

// crewEscalationPayload is a schema-complete escalation payload. Each named
// member can be deleted to isolate ONE missing required field.
func crewEscalationPayload(omit string) string {
	fields := map[string]string{
		"summary":             `"summary":"the planner and the reviewer disagree on the scope"`,
		"recommended_default": `"recommended_default":"scope it to the three named files"`,
		"tradeoffs":           `"tradeoffs":"a wider scope risks the file cap; a narrower one defers the coupling"`,
	}
	delete(fields, omit)
	var parts []string
	for _, k := range []string{"summary", "recommended_default", "tradeoffs"} {
		if v, ok := fields[k]; ok {
			parts = append(parts, v)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TestSendCrewMessage_EscalationWithoutRecommendedDefaultRefused is AC2's
// send-time contract: an escalation missing recommended_default (or tradeoffs)
// is refused with a 4xx whose error NAMES the missing field. Each case deletes
// exactly ONE member and leaves the other PRESENT, so the refusal is
// attributable to the deleted field and not to a second missing one — and the
// complete document is the PASS control, proving the fixture is not refused for
// an unrelated reason.
func TestSendCrewMessage_EscalationWithoutRecommendedDefaultRefused(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	send := func(payload string) *httptest.ResponseRecorder {
		return crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "",
			crewDoc("escalation", "", "captain", runAnchor(planRun.ID), payload),
			runBound(planRun.ID, "mcp:read", scopeWriteMessages))
	}
	for _, field := range []string{"recommended_default", "tradeoffs"} {
		t.Run("missing "+field, func(t *testing.T) {
			w := send(crewEscalationPayload(field))
			requireCrewRefusal(t, w, http.StatusBadRequest, "validation_failed")
			if !strings.Contains(w.Body.String(), field) {
				t.Errorf("the refusal does not name %q: %s", field, w.Body.String())
			}
		})
	}
	// The complete document reaches the mailbox — which has a NIL pool here, so a
	// panic-free 4xx/5xx from BEYOND the validator is the proof it passed
	// validation. Assert only that it is NOT the named validation refusal.
	t.Run("complete document passes validation", func(t *testing.T) {
		defer func() {
			// A nil-pool mailbox panics on the real send; recovering here keeps the
			// assertion about validation rather than about the pool.
			_ = recover()
		}()
		w := send(crewEscalationPayload(""))
		if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "recommended_default") {
			t.Errorf("a complete escalation was refused for recommended_default: %s", w.Body.String())
		}
	})
}

// TestNotifyRootEscalationPage_OnlyRootEscalationNotifies is binding approval
// condition 2 as a unit: the send-time notify condition MATCHES the projection.
// The threaded-reply row differs from the root row ONLY in the threadRoot
// argument, so the root condition is the sole discriminator; every other row is
// a named guard of the helper.
func TestNotifyRootEscalationPage_OnlyRootEscalationNotifies(t *testing.T) {
	runID := uuid.New()
	root := int64(40)
	escalation := &crewmessage.Message{Type: crewmessage.TypeEscalation}
	consult := &crewmessage.Message{Type: crewmessage.TypeConsult}
	runRow := &crewmessage.Row{SentSequence: 41, RunID: &runID}
	anchorless := &crewmessage.Row{SentSequence: 41}

	cases := map[string]struct {
		msg        *crewmessage.Message
		threadRoot *int64
		row        *crewmessage.Row
		want       bool
	}{
		"root escalation on a run":        {escalation, nil, runRow, true},
		"THREADED escalation reply":       {escalation, &root, runRow, false},
		"root consult":                    {consult, nil, runRow, false},
		"escalation on a run-less anchor": {escalation, nil, anchorless, false},
		"nil message":                     {nil, nil, runRow, false},
		"nil row (send failed)":           {escalation, nil, nil, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(Config{Addr: "127.0.0.1:0"})
			rec := &pageClassRecorder{}
			s.issueNotifier = rec
			s.notifyRootEscalationPage(context.Background(), tc.msg, tc.threadRoot, tc.row)
			if got := len(rec.pageClass) > 0; got != tc.want {
				t.Errorf("notified = %v, want %v (recorded %v)", got, tc.want, rec.pageClass)
			}
		})
	}
}

// TestCrewMessageResponse_WorkItemMembersOmitted (E77.7 / #3741): the two
// work-request members are omitempty, so every non-work_request send response
// stays byte-identical to E77.5's, and each surfaces when set.
func TestCrewMessageResponse_WorkItemMembersOmitted(t *testing.T) {
	plain, err := json.Marshal(crewMessageResponse{SentSequence: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{`"work_item"`, `"work_item_filing_error"`} {
		if strings.Contains(string(plain), k) {
			t.Errorf("an unset %s member serialised: %s", k, plain)
		}
	}
	set, _ := json.Marshal(crewMessageResponse{
		WorkItem:            &deferFiledIssue{Number: 7},
		WorkItemFilingError: &crewWorkItemFilingError{Code: "work_item_filing_failed", Message: "m"},
	})
	for _, k := range []string{`"work_item":{`, `"work_item_filing_error":{"code":"work_item_filing_failed"`} {
		if !strings.Contains(string(set), k) {
			t.Errorf("response %s lacks %s", set, k)
		}
	}
}

// --- E77.8 (#3742): the consult's account scope is part of the request -----

// withCorruptAccount injects a run-bound identity whose workspace account id
// is NOT a UUID — the corrupted-sessions-invariant state.
func withCorruptAccount(runID uuid.UUID, account string) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		r = withRunBoundIdentity(r, runID, "mcp:read", scopeWriteMessages)
		id := IdentityFrom(r.Context())
		id.AccountID = account
		return injectIdentity(r, id)
	}
}

// TestCrewMessageAPI_ConsultRefusesCorruptedAccountID is the counterfactual
// vehicle for the send path's fail-closed account resolve (E77.8 / #3742): a
// responder reads the decision index under CrewConsultRequest.AccountID, so
// silently degrading an undecodable account id to nil would WIDEN the read to
// every untenanted row.
//
// The two arms differ ONLY in the account value (same run, same body, same
// empty registry), so the 500 is attributable to the guard and not to a
// second difference: deleting the guard makes the corrupted arm fall through
// to the same 422 the valid arm answers.
func TestCrewMessageAPI_ConsultRefusesCorruptedAccountID(t *testing.T) {
	s, planRun, _ := crewNoDBServer(t)
	body := `{"schema_version":"crew-message-v1","type":"consult","recipient_role":"historian","anchor":` +
		runAnchor(planRun.ID) + `,"payload":` + crewConsultPayload + `,"response_required":true}`
	send := func(account string) *httptest.ResponseRecorder {
		return crewCall(t, s.handleSendCrewMessage, http.MethodPost, "/v0/crew-messages", "", body,
			withCorruptAccount(planRun.ID, account))
	}
	requireCrewRefusal(t, send("not-a-uuid"), http.StatusInternalServerError, "internal_error")
	// The control: a WELL-FORMED account on the same fixture is not refused by
	// this guard — it reaches the (empty) responder registry instead.
	requireCrewRefusal(t, send(uuid.NewString()), http.StatusUnprocessableEntity, "crew_responder_unavailable")
	// And an ABSENT account is legal (the untenanted posture), also reaching
	// the registry.
	requireCrewRefusal(t, send(""), http.StatusUnprocessableEntity, "crew_responder_unavailable")
}
