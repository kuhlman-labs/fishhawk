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
