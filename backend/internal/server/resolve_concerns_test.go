package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
)

// resolveServer wires the audit + concern fakes the resolve handler needs.
func resolveServer(t *testing.T) (*Server, *auditFake, *fakeConcernRepo) {
	t.Helper()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})
	return s, au, cr
}

func postResolve(t *testing.T, s *Server, runID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return postResolveAs(t, s, runID, body, withAuth)
}

func postResolveAs(t *testing.T, s *Server, runID string, body any, auth func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case string:
		raw = []byte(b)
	default:
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/concerns/resolve", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	s.handleResolveConcerns(w, auth(req))
	return w
}

func decodeResolve(t *testing.T, w *httptest.ResponseRecorder) resolveConcernsResponse {
	t.Helper()
	var resp resolveConcernsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resolve body: %v\n%s", err, w.Body.String())
	}
	return resp
}

// seedRoutedConcern seeds a concern and routes it to addressed_pending, the
// state a fix-up leaves it in.
func seedResolvableConcern(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID, note string) *concern.Concern {
	t.Helper()
	row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, note)
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{row.ID}, "routed by fix-up"); err != nil {
		t.Fatalf("route concern: %v", err)
	}
	return row
}

// identityAs injects an arbitrary identity.
func identityAs(id Identity) func(*http.Request) *http.Request {
	return func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	}
}

// TestResolveConcerns_LeavesAddressedNotWaived is the done-means (D3): a
// concern routed by a fix-up carrying operator_evidence, resolved through the
// verb, reads back from the store as `addressed` — NOT waived — with one
// concern_resolved_with_evidence row carrying the evidence and zero
// concern_waived rows.
func TestResolveConcerns_LeavesAddressedNotWaived(t *testing.T) {
	s, au, cr := resolveServer(t)
	runID := uuid.New()
	row := seedResolvableConcern(t, cr, runID, "nil deref in the merge path")
	// The routing pass that made the concern immune to reviewer auto-resolve.
	routing, _ := json.Marshal(map[string]any{
		"concern_ids":       []string{row.ID.String()},
		"operator_evidence": "reproduced the nil deref with the fixture",
	})
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &row.StageID, Timestamp: time.Now().UTC(),
		Category: CategoryStageFixupTriggered, Payload: routing,
	}); err != nil {
		t.Fatalf("seed routing entry: %v", err)
	}

	const evidence = "re-ran the reproduction at the fix-up head; the deref no longer fires"
	w := postResolve(t, s, runID.String(), resolveConcernsRequest{
		ConcernIDs: []string{row.ID.String()}, Evidence: evidence,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeResolve(t, w)
	if resp.Resolved != 1 || resp.Failed != 0 || len(resp.Results) != 1 || !resp.Results[0].Applied {
		t.Fatalf("resolve response = %+v, want 1 resolved", resp)
	}
	if resp.Results[0].State != string(concern.StateAddressed) {
		t.Errorf("results[0].state = %q, want addressed", resp.Results[0].State)
	}

	got, err := cr.GetByIDs(context.Background(), []uuid.UUID{row.ID})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got[0].State != concern.StateAddressed {
		t.Errorf("stored state = %q, want addressed (not waived)", got[0].State)
	}
	if got[0].StateReason != resolveConcernsReasonPrefix+evidence {
		t.Errorf("stored state_reason = %q, want the prefixed evidence", got[0].StateReason)
	}

	intents := auditEntriesByCategory(au, CategoryConcernResolvedWithEvidence)
	if len(intents) != 1 {
		t.Fatalf("concern_resolved_with_evidence entries = %d, want 1", len(intents))
	}
	entry := au.appended[intents[0]]
	var payload map[string]any
	if err := json.Unmarshal(entry.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for k, want := range map[string]string{
		"concern_id":  row.ID.String(),
		"prior_state": string(concern.StateAddressedPending),
		"evidence":    evidence,
		"stage_kind":  concern.StageKindImplement,
		"severity":    "medium",
		"category":    "scope",
	} {
		if payload[k] != want {
			t.Errorf("payload[%s] = %v, want %q", k, payload[k], want)
		}
	}
	if _, ok := payload["provenance"]; ok {
		t.Errorf("payload carries provenance for a reviewer concern: %s", entry.Payload)
	}
	if entry.ActorKind == nil || *entry.ActorKind != audit.ActorUser {
		t.Errorf("actor kind = %v, want user", entry.ActorKind)
	}
	if entry.ActorSubject == nil || *entry.ActorSubject != testOperatorIdentity().Subject {
		t.Errorf("actor subject = %v, want the operator", entry.ActorSubject)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0 — a resolve is not a waive", n)
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernResolveFailed)); n != 0 {
		t.Errorf("concern_resolve_failed entries = %d, want 0", n)
	}
}

// TestResolveConcerns_StampsProvenance: a server_check concern's provenance is
// stamped on the intent entry, like the waive.
func TestResolveConcerns_StampsProvenance(t *testing.T) {
	s, au, cr := resolveServer(t)
	runID := uuid.New()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: runID, StageID: uuid.New(), StageKind: concern.StageKindImplement,
		OriginReviewSequence: 1, Provenance: concern.ProvenanceServerCheck,
		Concerns: []concern.RaisedConcern{{Severity: "high", Category: "security", Note: "secret"}},
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := cr.MarkAddressedPending(context.Background(), []uuid.UUID{rows[0].ID}, "routed"); err != nil {
		t.Fatalf("route: %v", err)
	}
	w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{rows[0].ID.String()}, Evidence: "rotated and removed"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	intents := auditEntriesByCategory(au, CategoryConcernResolvedWithEvidence)
	if len(intents) != 1 {
		t.Fatalf("intent entries = %d, want 1", len(intents))
	}
	if !strings.Contains(string(au.appended[intents[0]].Payload), `"provenance":"`+concern.ProvenanceServerCheck+`"`) {
		t.Errorf("payload = %s, want provenance %q", au.appended[intents[0]].Payload, concern.ProvenanceServerCheck)
	}
}

// TestResolveConcerns_PreValidation is the all-or-nothing ladder. Every row
// reads COMMITTED state after the call: zero concern_resolved_with_evidence
// rows and every seeded concern's state unchanged — so a deleted guard that
// lets the request through (and the state machine then refuses) still goes
// RED on the intent-row count.
func TestResolveConcerns_PreValidation(t *testing.T) {
	type fixture struct {
		runID uuid.UUID
		good  *concern.Concern // a valid addressed_pending concern of runID
		cr    *fakeConcernRepo
	}
	overCap := make([]string, resolveConcernsMaxConcerns+1)
	for i := range overCap {
		overCap[i] = uuid.NewString()
	}

	cases := []struct {
		name     string
		auth     func(*http.Request) *http.Request
		body     func(f fixture) any
		wantCode int
		wantErr  string
		wantRule string // details.rule or details.field, when asserted
	}{
		{
			name:     "blank evidence",
			body:     func(f fixture) any { return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String()}} },
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "evidence",
		},
		{
			name: "whitespace evidence",
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String()}, Evidence: " \n\t "}
			},
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "evidence",
		},
		{
			name: "evidence over the cap",
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String()}, Evidence: strings.Repeat("e", maxOperatorConcernBytes+1)}
			},
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "evidence",
		},
		{
			name:     "empty list",
			body:     func(fixture) any { return resolveConcernsRequest{Evidence: "e"} },
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "concern_ids",
		},
		{
			name: "51 ids",
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: append([]string{f.good.ID.String()}, overCap[1:]...), Evidence: "e"}
			},
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "concern_ids",
		},
		{
			name: "non-UUID id",
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String(), "not-a-uuid"}, Evidence: "e"}
			},
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "concern_ids",
		},
		{
			name: "one id in two spellings",
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String(), strings.ToUpper(f.good.ID.String())}, Evidence: "e"}
			},
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "concern_ids",
		},
		{
			name:     "invalid JSON",
			body:     func(fixture) any { return "{not json" },
			wantCode: http.StatusBadRequest, wantErr: "validation_failed",
		},
		{
			name: "unknown id",
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String(), uuid.NewString()}, Evidence: "e"}
			},
			wantCode: http.StatusNotFound, wantErr: "concern_not_found",
		},
		{
			name: "id from another run",
			body: func(f fixture) any {
				other := seedResolvableConcern(t, f.cr, uuid.New(), "other run")
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String(), other.ID.String()}, Evidence: "e"}
			},
			wantCode: http.StatusBadRequest, wantErr: "validation_failed", wantRule: "concern_run_mismatch",
		},
		{
			name: "auth: anonymous",
			auth: func(req *http.Request) *http.Request { return req },
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String()}, Evidence: "e"}
			},
			wantCode: http.StatusUnauthorized, wantErr: "authentication_required",
		},
		{
			name: "auth: missing scope",
			auth: identityAs(Identity{Subject: "github:reader", TokenID: "tok-read", Scopes: []string{"read:runs"}}),
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String()}, Evidence: "e"}
			},
			wantCode: http.StatusForbidden, wantErr: "insufficient_scope",
		},
		{
			name: "auth: operator-agent token",
			auth: identityAs(Identity{Subject: operatorAgentSubject, TokenID: "tok-agent", Scopes: []string{"write:stages"}}),
			body: func(f fixture) any {
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String()}, Evidence: "e"}
			},
			wantCode: http.StatusForbidden, wantErr: errCodeResolveRequiresHuman,
		},
	}
	// State rows: a VALID addressed_pending concern first, the conflicting one
	// second, so the read-back also proves the first was not resolved.
	for _, st := range []concern.State{concern.StateRaised, concern.StateReopened, concern.StateAddressed, concern.StateWaived} {
		st := st
		cases = append(cases, struct {
			name     string
			auth     func(*http.Request) *http.Request
			body     func(f fixture) any
			wantCode int
			wantErr  string
			wantRule string
		}{
			name: "state " + string(st),
			body: func(f fixture) any {
				bad := seedConcernRow(t, f.cr, f.runID, uuid.New(), concern.StageKindImplement, 2, "bad")
				bad.State = st
				return resolveConcernsRequest{ConcernIDs: []string{f.good.ID.String(), bad.ID.String()}, Evidence: "e"}
			},
			wantCode: http.StatusConflict, wantErr: errCodeConcernResolveConflict,
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, au, cr := resolveServer(t)
			runID := uuid.New()
			f := fixture{runID: runID, good: seedResolvableConcern(t, cr, runID, "good"), cr: cr}
			body := tc.body(f)
			// Snapshot every seeded row's state AFTER the case seeded its own.
			before := map[uuid.UUID]concern.State{}
			for _, row := range cr.rows {
				before[row.ID] = row.State
			}
			auth := tc.auth
			if auth == nil {
				auth = withAuth
			}
			w := postResolveAs(t, s, runID.String(), body, auth)
			if w.Code != tc.wantCode {
				t.Errorf("status = %d, want %d:\n%s", w.Code, tc.wantCode, w.Body.String())
			}
			code, details := bulkWaiveErrorEnvelope(t, w)
			if code != tc.wantErr {
				t.Errorf("error code = %q, want %q", code, tc.wantErr)
			}
			if tc.wantRule != "" && details["rule"] != tc.wantRule && details["field"] != tc.wantRule {
				t.Errorf("details = %v, want rule/field %q", details, tc.wantRule)
			}
			if n := len(auditEntriesByCategory(au, CategoryConcernResolvedWithEvidence)); n != 0 {
				t.Errorf("concern_resolved_with_evidence entries = %d, want 0 (pre-validation mutates nothing)", n)
			}
			if n := len(auditEntriesByCategory(au, CategoryConcernResolveFailed)); n != 0 {
				t.Errorf("concern_resolve_failed entries = %d, want 0", n)
			}
			assertConcernStates(t, cr, before)
		})
	}
}

// TestResolveConcerns_ConflictMessageNamesRecovery pins the 409's recovery
// wording for an open-but-unrouted state vs a closed one.
func TestResolveConcerns_ConflictMessageNamesRecovery(t *testing.T) {
	for st, want := range map[concern.State]string{
		concern.StateRaised:    "route it with a fix-up",
		concern.StateReopened:  "route it with a fix-up",
		concern.StateAddressed: "nothing to resolve",
		concern.StateWaived:    "nothing to resolve",
	} {
		s, _, cr := resolveServer(t)
		runID := uuid.New()
		row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "x")
		row.State = st
		w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"})
		env := decodeErrorEnvelope(t, w)
		if !strings.Contains(env.Message, want) || !strings.Contains(env.Message, string(st)) {
			t.Errorf("state %s: message = %q, want it to name the state and %q", st, env.Message, want)
		}
	}
}

// TestResolveConcerns_RunBoundTokenOwnRun_Refused: a run-bound mcp:run: token
// is an agent even on its OWN run, so the verb refuses it.
func TestResolveConcerns_RunBoundTokenOwnRun_Refused(t *testing.T) {
	s, au, cr := resolveServer(t)
	runID := uuid.New()
	row := seedResolvableConcern(t, cr, runID, "a")
	w := postResolveAs(t, s, runID.String(),
		resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"},
		withRunBoundAuth(runID))
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code, _ := bulkWaiveErrorEnvelope(t, w); code != errCodeResolveRequiresHuman {
		t.Errorf("error code = %q, want %s", code, errCodeResolveRequiresHuman)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{row.ID: concern.StateAddressedPending})
	if n := len(auditEntriesByCategory(au, CategoryConcernResolvedWithEvidence)); n != 0 {
		t.Errorf("intent entries = %d, want 0", n)
	}
}

func TestResolveConcerns_MalformedRunID_Refused(t *testing.T) {
	s, _, _ := resolveServer(t)
	w := postResolve(t, s, "not-a-uuid", resolveConcernsRequest{ConcernIDs: []string{uuid.NewString()}, Evidence: "e"})
	code, details := bulkWaiveErrorEnvelope(t, w)
	if w.Code != http.StatusBadRequest || code != "validation_failed" || details["field"] != "run_id" {
		t.Errorf("status/code/details = %d/%q/%v, want 400 validation_failed field=run_id", w.Code, code, details)
	}
}

// TestResolveConcerns_StoreUnwired_Returns503 pins the unwired-store refusal.
func TestResolveConcerns_StoreUnwired_Returns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: newAuditFake()})
	w := postResolve(t, s, uuid.NewString(), resolveConcernsRequest{ConcernIDs: []string{uuid.NewString()}, Evidence: "e"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code, _ := bulkWaiveErrorEnvelope(t, w); code != "concern_store_unconfigured" {
		t.Errorf("code = %q, want concern_store_unconfigured", code)
	}
}

// TestResolveConcerns_ConcernReadError_Returns500: a non-NotFound read failure
// is a 500, and nothing is appended.
func TestResolveConcerns_ConcernReadError_Returns500(t *testing.T) {
	s, au, cr := resolveServer(t)
	cr.getByIDsErr = errors.New("db down")
	w := postResolve(t, s, uuid.NewString(), resolveConcernsRequest{ConcernIDs: []string{uuid.NewString()}, Evidence: "e"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if n := len(auditEntriesByCategory(au, CategoryConcernResolvedWithEvidence)); n != 0 {
		t.Errorf("intent entries = %d, want 0", n)
	}
}

// TestResolveConcerns_AuditAppendFailure_LeavesStateUnchanged: with the intent
// append failing, the item fails audit_append_failed and nothing transitions.
func TestResolveConcerns_AuditAppendFailure_LeavesStateUnchanged(t *testing.T) {
	s, au, cr := resolveServer(t)
	au.appendErrCategory = CategoryConcernResolvedWithEvidence
	runID := uuid.New()
	row := seedResolvableConcern(t, cr, runID, "a")

	w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-item failure):\n%s", w.Code, w.Body.String())
	}
	resp := decodeResolve(t, w)
	if resp.Resolved != 0 || resp.Failed != 1 || resp.Results[0].Applied || resp.Results[0].ErrorCode != "audit_append_failed" {
		t.Errorf("resp = %+v, want one audit_append_failed item", resp)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{row.ID: concern.StateAddressedPending})
	if n := len(auditEntriesByCategory(au, CategoryConcernResolveFailed)); n != 0 {
		t.Errorf("concern_resolve_failed entries = %d, want 0 — there is no intent to correct", n)
	}
}

// TestResolveConcerns_MidBatchTransitionFailure is the partial-application
// contract: a three-item batch whose SECOND item loses a race after its intent
// entry reports resolved=2 / failed=1 in REQUEST order, writes ONE corrective
// concern_resolve_failed row for that item, and still applies the third.
func TestResolveConcerns_MidBatchTransitionFailure(t *testing.T) {
	au := newAuditFake()
	base := newFakeConcernRepo()
	runID := uuid.New()
	a := seedResolvableConcern(t, base, runID, "a")
	b := seedResolvableConcern(t, base, runID, "b")
	c := seedResolvableConcern(t, base, runID, "c")
	cr := &midBatchFailConcernRepo{fakeConcernRepo: base, failFor: b.ID}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})

	w := postResolve(t, s, runID.String(), resolveConcernsRequest{
		ConcernIDs: []string{a.ID.String(), b.ID.String(), c.ID.String()}, Evidence: "e",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	resp := decodeResolve(t, w)
	if resp.Resolved != 2 || resp.Failed != 1 || len(resp.Results) != 3 {
		t.Fatalf("resolved/failed/results = %d/%d/%d, want 2/1/3: %+v", resp.Resolved, resp.Failed, len(resp.Results), resp)
	}
	for i, want := range []struct {
		id      uuid.UUID
		applied bool
	}{{a.ID, true}, {b.ID, false}, {c.ID, true}} {
		if resp.Results[i].ConcernID != want.id.String() || resp.Results[i].Applied != want.applied {
			t.Errorf("results[%d] = %+v, want %s applied=%v (request order)", i, resp.Results[i], want.id, want.applied)
		}
	}
	if resp.Results[1].ErrorCode != errCodeConcernResolveConflict {
		t.Errorf("results[1].error_code = %q, want %s", resp.Results[1].ErrorCode, errCodeConcernResolveConflict)
	}
	assertConcernStates(t, base, map[uuid.UUID]concern.State{
		a.ID: concern.StateAddressed, b.ID: concern.StateAddressedPending, c.ID: concern.StateAddressed,
	})
	if n := len(auditEntriesByCategory(au, CategoryConcernResolvedWithEvidence)); n != 3 {
		t.Errorf("intent entries = %d, want 3 (one per item, appended before each transition)", n)
	}
	failed := auditEntriesByCategory(au, CategoryConcernResolveFailed)
	if len(failed) != 1 {
		t.Fatalf("concern_resolve_failed entries = %d, want 1", len(failed))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[failed[0]].Payload, &payload); err != nil {
		t.Fatalf("decode corrective: %v", err)
	}
	if payload["concern_id"] != b.ID.String() || payload["intended_state"] != string(concern.StateAddressed) ||
		payload["actual_state"] != string(concern.StateWaived) {
		t.Errorf("corrective payload = %v, want b / intended addressed / actual waived", payload)
	}
}

// TestResolveConcerns_GenericTransitionError_InternalError: a transition
// failure that is not an InvalidTransitionError maps to internal_error, still
// with the corrective entry.
func TestResolveConcerns_GenericTransitionError_InternalError(t *testing.T) {
	s, au, cr := resolveServer(t)
	runID := uuid.New()
	row := seedResolvableConcern(t, cr, runID, "a")
	cr.applyResolutionErr = errors.New("db write failed")

	w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"})
	resp := decodeResolve(t, w)
	if resp.Failed != 1 || resp.Results[0].ErrorCode != "internal_error" {
		t.Errorf("resp = %+v, want one internal_error item", resp)
	}
	failed := auditEntriesByCategory(au, CategoryConcernResolveFailed)
	if len(failed) != 1 {
		t.Fatalf("concern_resolve_failed entries = %d, want 1", len(failed))
	}
	if !strings.Contains(string(au.appended[failed[0]].Payload), `"actual_state":"addressed_pending"`) {
		t.Errorf("corrective payload = %s, want the row's own state when the error names none", au.appended[failed[0]].Payload)
	}
}

// TestResolveConcerns_CorrectiveAppendFailure_Warns: the corrective entry is
// warn-only — when IT fails too, the item still reports its conflict and the
// failure is logged.
func TestResolveConcerns_CorrectiveAppendFailure_Warns(t *testing.T) {
	var logBuf bytes.Buffer
	au := newAuditFake()
	au.appendErrCategory = CategoryConcernResolveFailed
	base := newFakeConcernRepo()
	runID := uuid.New()
	row := seedResolvableConcern(t, base, runID, "a")
	cr := &midBatchFailConcernRepo{fakeConcernRepo: base, failFor: row.ID}
	s := New(Config{
		Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr,
		Logger: slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})

	w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"})
	resp := decodeResolve(t, w)
	if resp.Failed != 1 || resp.Results[0].ErrorCode != errCodeConcernResolveConflict {
		t.Errorf("resp = %+v, want one concern_resolve_conflict item", resp)
	}
	if !strings.Contains(logBuf.String(), "append corrective concern_resolve_failed entry failed") {
		t.Errorf("log = %q, want the corrective-append warning", logBuf.String())
	}
}

// TestResolveConcerns_Route_CrossAccount_Forbidden pins the account-ownership
// wrapper on the REGISTERED route (binding condition 1, the #4101/#4082 gap
// class): POST /v0/runs/{run_id}/concerns/resolve is registered as
// requireRunAccount(memberWrite, handleResolveConcerns), and a bearer bound to
// account A resolving a concern of a run owned by account B is 403
// account_forbidden with no audit row and no state change. It drives
// s.Handler().ServeHTTP with real bearers the auth middleware resolves, so the
// wrapper at the registration site is on the path.
//
// Isolation: both tokens are human subjects carrying write:stages, so neither
// insufficient_scope nor resolve_requires_human can mask a deleted wrapper, and
// the owner control proves the fixture reaches the handler (200, addressed) —
// with the wrapper gone the cross-account bearer would get that same 200.
func TestResolveConcerns_Route_CrossAccount_Forbidden(t *testing.T) {
	const (
		crossBearer = "fhk_resolve_account_a"
		ownerBearer = "fhk_resolve_account_b"
	)
	repo := newFakeRepo()
	cr := newFakeConcernRepo()
	au := newAuditFake()
	tokens := &multiTokenRepo{byPlain: map[string]*apitoken.Token{
		crossBearer: {ID: uuid.New(), Subject: "github:op-a", AccountID: authzAcctA, Scopes: []string{"write:stages"}, PlainText: crossBearer},
		ownerBearer: {ID: uuid.New(), Subject: "github:op-b", AccountID: authzAcctB, Scopes: []string{"write:stages"}, PlainText: ownerBearer},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr, APITokenRepo: tokens})

	runID := seedGateRun(t, repo)
	repo.mu.Lock()
	repo.runs[runID].AccountID = authzAcctB
	repo.mu.Unlock()
	crossRow := seedResolvableConcern(t, cr, runID, "cross-account target")
	ownerRow := seedResolvableConcern(t, cr, runID, "owner target")

	post := func(bearer string, row *concern.Concern) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "re-ran it"})
		req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID.String()+"/concerns/resolve", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+bearer)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w
	}

	w := post(crossBearer, crossRow)
	if w.Code != http.StatusForbidden || !bodyHasCode(w, "account_forbidden") {
		t.Errorf("cross-account status = %d, want 403 account_forbidden; body %s", w.Code, w.Body.String())
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{crossRow.ID: concern.StateAddressedPending})
	if n := len(auditEntriesByCategory(au, CategoryConcernResolvedWithEvidence)); n != 0 {
		t.Errorf("intent entries after the cross-account request = %d, want 0", n)
	}

	// Positive control: the owning account's bearer reaches the handler.
	if w := post(ownerBearer, ownerRow); w.Code != http.StatusOK {
		t.Fatalf("owner status = %d, want 200 (fixture must reach the handler):\n%s", w.Code, w.Body.String())
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{ownerRow.ID: concern.StateAddressed})
}

// resolveRefreshRecorder records each status refresh together with the
// concern's STORED state at refresh time, so a test can prove the refresh
// fires only after the batch's writes committed.
type resolveRefreshRecorder struct {
	*pageClassRecorder
	cr        *fakeConcernRepo
	concernID uuid.UUID
	seen      []concern.State
}

func (r *resolveRefreshRecorder) NotifyStatusUpdateForRun(ctx context.Context, runID uuid.UUID) error {
	if got, err := r.cr.GetByIDs(ctx, []uuid.UUID{r.concernID}); err == nil {
		r.seen = append(r.seen, got[0].State)
	}
	return r.pageClassRecorder.NotifyStatusUpdateForRun(ctx, runID)
}

// TestResolveConcerns_StatusRefresh pins the issue-thread wiring (approval
// condition 3, amendment 53d8b993): a batch that resolved >= 1 concern
// refreshes the status comment exactly once, AFTER the transition committed
// (the recorder reads `addressed` at refresh time), so the
// concern_resolved_with_evidence activity line reaches the thread. An
// all-failed batch — attempted 1, resolved 0 — and a refused batch refresh
// nothing. Counterfactuals: deleting the call reddens the resolving arm;
// gating on the attempted count reddens the all-failed arm.
func TestResolveConcerns_StatusRefresh(t *testing.T) {
	t.Run("resolving batch refreshes once after the write", func(t *testing.T) {
		s, _, cr := resolveServer(t)
		runID := uuid.New()
		row := seedResolvableConcern(t, cr, runID, "a")
		rec := &resolveRefreshRecorder{pageClassRecorder: &pageClassRecorder{}, cr: cr, concernID: row.ID}
		s.issueNotifier = rec

		w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"})
		if w.Code != http.StatusOK || decodeResolve(t, w).Resolved != 1 {
			t.Fatalf("status = %d, want 200 with one resolved:\n%s", w.Code, w.Body.String())
		}
		if len(rec.status) != 1 || rec.status[0] != runID {
			t.Fatalf("status refreshes = %v, want exactly one for run %s", rec.status, runID)
		}
		if len(rec.seen) != 1 || rec.seen[0] != concern.StateAddressed {
			t.Errorf("concern state at refresh = %v, want [addressed] (refresh after the write)", rec.seen)
		}
	})

	t.Run("all-failed batch refreshes nothing", func(t *testing.T) {
		s, au, cr := resolveServer(t)
		au.appendErrCategory = CategoryConcernResolvedWithEvidence
		runID := uuid.New()
		row := seedResolvableConcern(t, cr, runID, "a")
		rec := &pageClassRecorder{}
		s.issueNotifier = rec

		w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"})
		if resp := decodeResolve(t, w); w.Code != http.StatusOK || resp.Resolved != 0 || resp.Failed != 1 {
			t.Fatalf("status = %d resp = %+v, want 200 with one failed item", w.Code, resp)
		}
		if len(rec.status) != 0 {
			t.Errorf("status refreshes = %d, want 0 for a batch that resolved nothing", len(rec.status))
		}
	})

	t.Run("refused batch refreshes nothing", func(t *testing.T) {
		s, _, cr := resolveServer(t)
		runID := uuid.New()
		row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "raised, not routed")
		rec := &pageClassRecorder{}
		s.issueNotifier = rec

		w := postResolve(t, s, runID.String(), resolveConcernsRequest{ConcernIDs: []string{row.ID.String()}, Evidence: "e"})
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		if len(rec.status) != 0 {
			t.Errorf("status refreshes = %d, want 0 for a refused batch", len(rec.status))
		}
	})
}
