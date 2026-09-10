package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// bulkWaiveServer wires the audit + concern fakes the bulk handler needs.
func bulkWaiveServer(t *testing.T) (*Server, *auditFake, *fakeConcernRepo) {
	t.Helper()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		AuditRepo:   au,
		ConcernRepo: cr,
	})
	return s, au, cr
}

func postBulkWaive(t *testing.T, s *Server, runID string, body bulkWaiveRequest) *httptest.ResponseRecorder {
	t.Helper()
	return postBulkWaiveAs(t, s, runID, body, withAuth)
}

func postBulkWaiveAs(t *testing.T, s *Server, runID string, body bulkWaiveRequest, auth func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/"+runID+"/concerns/waive", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID)
	w := httptest.NewRecorder()
	s.handleBulkWaiveConcerns(w, auth(req))
	return w
}

// assertConcernStates reads every named concern back from the store AFTER the
// call returned and asserts its state. Reading COMMITTED STATE (rather than
// only the refusal's error identity) is what makes the pre-validation tests
// below working counterfactuals: a control that fires and a control that never
// fired can produce the same error, but only one leaves the OTHER batched
// concerns untouched.
func assertConcernStates(t *testing.T, cr *fakeConcernRepo, want map[uuid.UUID]concern.State) {
	t.Helper()
	for id, wantState := range want {
		rows, err := cr.GetByIDs(context.Background(), []uuid.UUID{id})
		if err != nil {
			t.Fatalf("read back concern %s: %v", id, err)
		}
		if rows[0].State != wantState {
			t.Errorf("concern %s state = %q, want %q", id, rows[0].State, wantState)
		}
	}
}

func decodeBulkWaive(t *testing.T, w *httptest.ResponseRecorder) bulkWaiveResponse {
	t.Helper()
	var resp bulkWaiveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode bulk waive body: %v\n%s", err, w.Body.String())
	}
	return resp
}

// bulkWaiveErrorEnvelope decodes {"error":{"code","details"}} from a refusal.
func bulkWaiveErrorEnvelope(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var wrapper map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &wrapper); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
	}
	env, _ := wrapper["error"].(map[string]any)
	if env == nil {
		t.Fatalf("body carries no error envelope: %s", w.Body.String())
	}
	code, _ := env["code"].(string)
	details, _ := env["details"].(map[string]any)
	return code, details
}

// TestBulkWaive_HappyPath_OneAuditRowPerConcern is the done-means: N concerns,
// N concern_waived audit rows each naming its OWN concern_id and the SHARED
// reason, every row terminal.
func TestBulkWaive_HappyPath_OneAuditRowPerConcern(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	b := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindPlan, 2, "b")
	c := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 3, "c")

	const reason = "answered by the approval condition; not blocking the merge"
	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), b.ID.String(), c.ID.String()},
		Reason:     reason,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeBulkWaive(t, w)
	if resp.Waived != 3 || resp.Failed != 0 {
		t.Errorf("waived/failed = %d/%d, want 3/0: %+v", resp.Waived, resp.Failed, resp)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("results = %d, want 3: %+v", len(resp.Results), resp.Results)
	}
	for i, want := range []*concern.Concern{a, b, c} {
		if resp.Results[i].ConcernID != want.ID.String() {
			t.Errorf("results[%d].concern_id = %s, want %s (request order)", i, resp.Results[i].ConcernID, want.ID)
		}
		if !resp.Results[i].Applied || resp.Results[i].State != string(concern.StateWaived) {
			t.Errorf("results[%d] = %+v, want applied waived", i, resp.Results[i])
		}
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{
		a.ID: concern.StateWaived, b.ID: concern.StateWaived, c.ID: concern.StateWaived,
	})

	idx := auditEntriesByCategory(au, CategoryConcernWaived)
	if len(idx) != 3 {
		t.Fatalf("concern_waived entries = %d, want 3", len(idx))
	}
	seenIDs := map[string]bool{}
	for _, i := range idx {
		var payload map[string]any
		if err := json.Unmarshal(au.appended[i].Payload, &payload); err != nil {
			t.Fatalf("decode concern_waived payload: %v", err)
		}
		cid, _ := payload["concern_id"].(string)
		seenIDs[cid] = true
		if payload["reason"] != reason {
			t.Errorf("entry for %s carries reason %v, want the shared batch reason", cid, payload["reason"])
		}
		if payload["bulk_waive"] != true {
			t.Errorf("entry for %s missing the bulk_waive marker: %v", cid, payload)
		}
	}
	for _, want := range []*concern.Concern{a, b, c} {
		if !seenIDs[want.ID.String()] {
			t.Errorf("no concern_waived entry names %s", want.ID)
		}
	}
}

func TestBulkWaive_BlankReason_Refused(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String()}, Reason: "   ",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a blank reason:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" || details["field"] != "reason" {
		t.Errorf("error = %q %v, want validation_failed field=reason", code, details)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0", n)
	}
}

func TestBulkWaive_EmptyConcernIDs_Refused(t *testing.T) {
	s, _, _ := bulkWaiveServer(t)
	w := postBulkWaive(t, s, uuid.NewString(), bulkWaiveRequest{Reason: "r"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on an empty concern_ids:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" || details["field"] != "concern_ids" {
		t.Errorf("error = %q %v, want validation_failed field=concern_ids", code, details)
	}
}

// TestBulkWaive_OverCap_Refused pins the 50-id cap: a NAMED 400 refusal, never
// a silent truncation, and nothing waived.
func TestBulkWaive_OverCap_Refused(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	ids := make([]string, 0, bulkWaiveMaxConcerns+1)
	states := map[uuid.UUID]concern.State{}
	for i := 0; i <= bulkWaiveMaxConcerns; i++ {
		row := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, int64(i), fmt.Sprintf("c%d", i))
		ids = append(ids, row.ID.String())
		states[row.ID] = concern.StateRaised
	}

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{ConcernIDs: ids, Reason: "r"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 over the cap:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" {
		t.Errorf("error code = %q, want validation_failed", code)
	}
	if n, _ := details["max"].(float64); int(n) != bulkWaiveMaxConcerns {
		t.Errorf("details.max = %v, want %d", details["max"], bulkWaiveMaxConcerns)
	}
	assertConcernStates(t, cr, states)
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0 (nothing waived over the cap)", n)
	}
}

// TestBulkWaive_DuplicateID_Refused: a repeated id would otherwise produce TWO
// concern_waived rows for one concern (the second failing the transition), so
// it is refused up front.
func TestBulkWaive_DuplicateID_Refused(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), a.ID.String()}, Reason: "r",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a duplicate id:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" || details["concern_id"] != a.ID.String() {
		t.Errorf("error = %q %v, want validation_failed naming %s", code, details, a.ID)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0 (the duplicate must not waive twice)", n)
	}
}

// TestBulkWaive_DuplicateUUIDSpelling_Refused hardens the test above past a
// byte-identical repeat. uuid.Parse accepts SEVERAL spellings of one id, so a
// raw-string duplicate check would pass a batch naming the same concern twice
// in two spellings and then append TWO concern_waived rows for it — the second
// failing its transition mid-batch AFTER the first already committed. Each
// case pairs one spelling with an EQUIVALENT one (never with a different
// concern), so the refusal can only come from the equivalence check itself,
// and state is read back after the call because the control's real effect is
// that NOTHING was waived.
func TestBulkWaive_DuplicateUUIDSpelling_Refused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alias func(uuid.UUID) string
	}{
		{"uppercase", func(id uuid.UUID) string { return strings.ToUpper(id.String()) }},
		{"urn_prefix", func(id uuid.UUID) string { return "urn:uuid:" + id.String() }},
		{"braced", func(id uuid.UUID) string { return "{" + id.String() + "}" }},
		{"dashless_hex", func(id uuid.UUID) string { return strings.ReplaceAll(id.String(), "-", "") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, au, cr := bulkWaiveServer(t)
			runID := uuid.New()
			a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
			spelling := tc.alias(a.ID)
			if spelling == a.ID.String() {
				t.Fatalf("alias %q is byte-identical to the canonical form; the case proves nothing", spelling)
			}

			w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
				ConcernIDs: []string{a.ID.String(), spelling}, Reason: "r",
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 on an equivalent UUID spelling:\n%s", w.Code, w.Body.String())
			}
			code, details := bulkWaiveErrorEnvelope(t, w)
			if code != "validation_failed" {
				t.Errorf("error code = %q, want validation_failed", code)
			}
			if details["concern_id"] != spelling {
				t.Errorf("details.concern_id = %v, want the raw spelling %q", details["concern_id"], spelling)
			}
			if details["canonical_concern_id"] != a.ID.String() {
				t.Errorf("details.canonical_concern_id = %v, want %s", details["canonical_concern_id"], a.ID)
			}
			// The refusal must land BEFORE any audit append, so the concern is
			// still open and no row was written.
			assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
			if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
				t.Errorf("concern_waived entries = %d, want 0 (the refusal precedes every append)", n)
			}
		})
	}
}

func TestBulkWaive_NonUUIDID_Refused(t *testing.T) {
	s, _, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), "not-a-uuid"}, Reason: "r",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a non-UUID id:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" || details["concern_id"] != "not-a-uuid" {
		t.Errorf("error = %q %v, want validation_failed naming the malformed id", code, details)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
}

func TestBulkWaive_StoreUnwired_Returns503(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: newAuditFake()}) // no ConcernRepo
	w := postBulkWaive(t, s, uuid.NewString(), bulkWaiveRequest{
		ConcernIDs: []string{uuid.NewString()}, Reason: "r",
	})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code, _ := bulkWaiveErrorEnvelope(t, w); code != "concern_store_unconfigured" {
		t.Errorf("error code = %q, want concern_store_unconfigured", code)
	}
}

func TestBulkWaive_UnknownID_Returns404(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	missing := uuid.NewString()

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), missing}, Reason: "r",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 on an unknown id:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "concern_not_found" || details["concern_id"] != missing {
		t.Errorf("error = %q %v, want concern_not_found naming %s", code, details, missing)
	}
	// The whole batch is refused: the resolvable sibling is untouched.
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0", n)
	}
}

// TestBulkWaive_ConcernFromAnotherRun_Refused pins the cross-run RunID check.
// The foreign concern is seeded BY CONSTRUCTION under a second, freshly-minted
// run id — definitionally foreign, never by calling the control inside the
// test's own setup — so the RED lands on the behavioral assertion rather than a
// fixture-setup failure. It reads the FIRST run's concern states after the call
// because the control's effect is COMMITTED STATE: a batch that proceeded would
// have waived the sibling.
//
// Per the house convention (verified against the acceptance-amendment
// refusals), the top-level code is validation_failed and the SPECIFIC rule
// travels in details as "concern_run_mismatch".
func TestBulkWaive_ConcernFromAnotherRun_Refused(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	b := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 2, "b")
	otherRunID := uuid.New()
	foreign := seedConcernRow(t, cr, otherRunID, uuid.New(), concern.StageKindImplement, 1, "another run's concern")

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), foreign.ID.String(), b.ID.String()}, Reason: "r",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a cross-run id:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" {
		t.Errorf("error code = %q, want validation_failed (the house convention; the specific rule rides details.rule)", code)
	}
	if details["rule"] != "concern_run_mismatch" {
		t.Errorf("details.rule = %v, want concern_run_mismatch", details["rule"])
	}
	if details["concern_id"] != foreign.ID.String() {
		t.Errorf("details.concern_id = %v, want the foreign id %s", details["concern_id"], foreign.ID)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{
		a.ID: concern.StateRaised, b.ID: concern.StateRaised, foreign.ID: concern.StateRaised,
	})
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0", n)
	}
}

// TestBulkWaive_NotOpenConcern_RefusesWholeBatch: the pre-validation is
// all-or-nothing, so a single already-terminal id leaves every OTHER batched
// concern untouched. Asserted by reading committed state, not error identity.
func TestBulkWaive_NotOpenConcern_RefusesWholeBatch(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	settled := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 2, "already settled")
	b := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 3, "b")
	if _, err := cr.ApplyResolution(context.Background(), settled.ID, concern.StateWaived, "earlier"); err != nil {
		t.Fatalf("seed settled concern: %v", err)
	}

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), settled.ID.String(), b.ID.String()}, Reason: "r",
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 on a non-open id:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "concern_waive_conflict" || details["concern_id"] != settled.ID.String() {
		t.Errorf("error = %q %v, want concern_waive_conflict naming %s", code, details, settled.ID)
	}
	if details["from"] != string(concern.StateWaived) || details["to"] != string(concern.StateWaived) {
		t.Errorf("details from/to = %v/%v, want the waived/waived pair", details["from"], details["to"])
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{
		a.ID: concern.StateRaised, b.ID: concern.StateRaised, settled.ID: concern.StateWaived,
	})
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0 (nothing may be waived when the batch is refused)", n)
	}
}

// TestBulkWaive_RunBoundTokenCrossRun_Forbidden pins the mcp:run: subject
// binding against the PATH run id.
func TestBulkWaive_RunBoundTokenCrossRun_Forbidden(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	tokenRunID := uuid.New() // a DIFFERENT run

	w := postBulkWaiveAs(t, s, runID.String(),
		bulkWaiveRequest{ConcernIDs: []string{a.ID.String()}, Reason: "r"},
		func(req *http.Request) *http.Request { return withMCPFixupAuth(req, tokenRunID) })
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code, _ := bulkWaiveErrorEnvelope(t, w); code != "cross_run_waive" {
		t.Errorf("error code = %q, want cross_run_waive", code)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0", n)
	}
}

// TestBulkWaive_RunBoundTokenOwnRun_Succeeds is the paired positive: the same
// guard admits a token bound to the PATH run, so the 403 above is the guard
// firing rather than the route being unreachable under an MCP identity.
func TestBulkWaive_RunBoundTokenOwnRun_Succeeds(t *testing.T) {
	s, _, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")

	w := postBulkWaiveAs(t, s, runID.String(),
		bulkWaiveRequest{ConcernIDs: []string{a.ID.String()}, Reason: "own-run bulk waive"},
		func(req *http.Request) *http.Request { return withMCPFixupAuth(req, runID) })
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateWaived})
}

func TestBulkWaive_UnauthenticatedReturns401(t *testing.T) {
	s, _, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")

	w := postBulkWaiveAs(t, s, runID.String(),
		bulkWaiveRequest{ConcernIDs: []string{a.ID.String()}, Reason: "r"},
		func(req *http.Request) *http.Request { return req }) // no identity → anonymous
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
}

func TestBulkWaive_MissingScopeReturns403(t *testing.T) {
	s, _, cr := bulkWaiveServer(t)
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")

	w := postBulkWaiveAs(t, s, runID.String(),
		bulkWaiveRequest{ConcernIDs: []string{a.ID.String()}, Reason: "r"},
		func(req *http.Request) *http.Request {
			id := Identity{Subject: "token:reader", TokenID: "tok-read", Scopes: []string{"read:runs"}}
			return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code, _ := bulkWaiveErrorEnvelope(t, w); code != "insufficient_scope" {
		t.Errorf("error code = %q, want insufficient_scope", code)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{a.ID: concern.StateRaised})
}

func TestBulkWaive_MalformedRunID_Refused(t *testing.T) {
	s, _, _ := bulkWaiveServer(t)
	w := postBulkWaive(t, s, "not-a-uuid", bulkWaiveRequest{
		ConcernIDs: []string{uuid.NewString()}, Reason: "r",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on a malformed run_id:\n%s", w.Code, w.Body.String())
	}
	code, details := bulkWaiveErrorEnvelope(t, w)
	if code != "validation_failed" || details["field"] != "run_id" {
		t.Errorf("error = %q %v, want validation_failed field=run_id", code, details)
	}
}

// TestBulkWaive_Delegated_NotConfigured: the delegation check runs ONCE for the
// run, BEFORE any intent entry is appended — a refusal leaves every concern
// untouched.
func TestBulkWaive_Delegated_NotConfigured(t *testing.T) {
	s, au, cr := bulkWaiveServer(t) // no RunRepo → no operator_agent block resolvable
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	b := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 2, "b")

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), b.ID.String()}, Reason: "r", Delegated: true,
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code, _ := bulkWaiveErrorEnvelope(t, w); code != "delegation_not_configured" {
		t.Errorf("error code = %q, want delegation_not_configured", code)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{
		a.ID: concern.StateRaised, b.ID: concern.StateRaised,
	})
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0 (the delegation check precedes every append)", n)
	}
}

// TestBulkWaive_Delegated_SoloLowMet_StampsRuleOnEveryRow drives the delegated
// SUCCESS arm the refusal test above cannot reach: checkDelegation RESOLVES,
// and the returned rule must be stamped as the `delegated` key on EVERY
// concern_waived row the batch appends (applyConcernWaive writes it when
// delegatedRule != ""). The single-waive tests pin the stamping code itself;
// what is unwitnessed without this test is the BULK path's wiring of the
// once-per-run rule into the per-item apply loop.
//
// The batch is ONE concern by CONSTRUCTION, not by convenience: may_waive's
// only condition is solo_low, which requires the run to have EXACTLY ONE open
// concern and that concern to be low severity (delegation.evalSoloLow). A
// two-item delegated batch is therefore unsatisfiable by definition, so the
// assertion loops over every appended row and also pins the row COUNT — if a
// future condition makes a wider delegated batch reachable, the loop already
// covers it rather than silently checking only the first row.
func TestBulkWaive_Delegated_SoloLowMet_StampsRuleOnEveryRow(t *testing.T) {
	repo := newApprovalRunRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{
		Addr:        "127.0.0.1:0",
		RunRepo:     repo,
		AuditRepo:   au,
		ConcernRepo: cr,
	})
	runID, stageID := uuid.New(), uuid.New()
	row := seedLowConcernRow(t, cr, runID, stageID)
	repo.seedRun(&run.Run{
		ID:           runID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: []byte(delegatedActionSpecYAML),
	})

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{row.ID.String()}, Reason: "style nit, not blocking", Delegated: true,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 on a satisfied solo_low delegation:\n%s", w.Code, w.Body.String())
	}
	resp := decodeBulkWaive(t, w)
	if resp.Waived != 1 || resp.Failed != 0 {
		t.Errorf("waived/failed = %d/%d, want 1/0: %+v", resp.Waived, resp.Failed, resp)
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{row.ID: concern.StateWaived})

	idx := auditEntriesByCategory(au, CategoryConcernWaived)
	if len(idx) != 1 {
		t.Fatalf("concern_waived entries = %d, want 1 (solo_low admits exactly one open concern)", len(idx))
	}
	for _, i := range idx {
		var payload map[string]any
		if err := json.Unmarshal(au.appended[i].Payload, &payload); err != nil {
			t.Fatalf("decode concern_waived payload: %v", err)
		}
		// Presence and value asserted separately: an absent `delegated` key and
		// a present empty string both read as "" through a `v, _ :=` assertion,
		// and an absent key is exactly the regression this test exists to catch.
		raw, present := payload["delegated"]
		if !present {
			t.Fatalf("concern_waived payload carries NO delegated key; the once-per-run rule was not stamped: %v", payload)
		}
		if raw != "solo_low" {
			t.Errorf("delegated = %#v, want %q", raw, "solo_low")
		}
	}
}

// TestBulkWaive_AuditAppendFailure_LeavesConcernOpen pins durable-record-first
// through the SHARED helper: with the audit append failing, no concern
// transitions. State is read back after the call because that is the control's
// actual effect.
func TestBulkWaive_AuditAppendFailure_LeavesConcernOpen(t *testing.T) {
	s, au, cr := bulkWaiveServer(t)
	au.appendErr = errors.New("db down")
	runID := uuid.New()
	a := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	b := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 2, "b")

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), b.ID.String()}, Reason: "r",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-item failures are reported in the body):\n%s", w.Code, w.Body.String())
	}
	resp := decodeBulkWaive(t, w)
	if resp.Waived != 0 || resp.Failed != 2 {
		t.Errorf("waived/failed = %d/%d, want 0/2: %+v", resp.Waived, resp.Failed, resp)
	}
	for i, item := range resp.Results {
		if item.Applied || item.ErrorCode != "audit_append_failed" {
			t.Errorf("results[%d] = %+v, want applied=false error_code=audit_append_failed", i, item)
		}
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{
		a.ID: concern.StateRaised, b.ID: concern.StateRaised,
	})
}

// midBatchFailConcernRepo fails ApplyResolution for ONE id, simulating a
// concurrent transition that raced the pre-validation.
type midBatchFailConcernRepo struct {
	*fakeConcernRepo
	failFor uuid.UUID
}

func (m *midBatchFailConcernRepo) ApplyResolution(ctx context.Context, id uuid.UUID, to concern.State, reason string) (*concern.Concern, error) {
	if id == m.failFor {
		return nil, concern.InvalidTransitionError{From: concern.StateWaived, To: to}
	}
	return m.fakeConcernRepo.ApplyResolution(ctx, id, to, reason)
}

// TestBulkWaive_MidBatchApplyFailure is the partial-application contract. A
// three-item batch whose SECOND item fails reports waived=2 / failed=1 — two
// items DID succeed — and the per-item results list in REQUEST order is
// asserted alongside the counts, because asserting only the aggregates would
// absorb a future off-by-one in either direction. The corrective
// concern_waive_failed row must be present for the failed item.
func TestBulkWaive_MidBatchApplyFailure(t *testing.T) {
	au := newAuditFake()
	base := newFakeConcernRepo()
	runID := uuid.New()
	a := seedConcernRow(t, base, runID, uuid.New(), concern.StageKindImplement, 1, "a")
	b := seedConcernRow(t, base, runID, uuid.New(), concern.StageKindImplement, 2, "b")
	c := seedConcernRow(t, base, runID, uuid.New(), concern.StageKindImplement, 3, "c")
	cr := &midBatchFailConcernRepo{fakeConcernRepo: base, failFor: b.ID}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, ConcernRepo: cr})

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{a.ID.String(), b.ID.String(), c.ID.String()}, Reason: "r",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	resp := decodeBulkWaive(t, w)
	if resp.Waived != 2 || resp.Failed != 1 {
		t.Errorf("waived/failed = %d/%d, want 2/1 (the second of three failed): %+v", resp.Waived, resp.Failed, resp)
	}
	if len(resp.Results) != 3 {
		t.Fatalf("results = %d, want 3: %+v", len(resp.Results), resp.Results)
	}
	wantApplied := []bool{true, false, true}
	wantIDs := []string{a.ID.String(), b.ID.String(), c.ID.String()}
	for i := range resp.Results {
		if resp.Results[i].ConcernID != wantIDs[i] {
			t.Errorf("results[%d].concern_id = %s, want %s (request order)", i, resp.Results[i].ConcernID, wantIDs[i])
		}
		if resp.Results[i].Applied != wantApplied[i] {
			t.Errorf("results[%d].applied = %v, want %v: %+v", i, resp.Results[i].Applied, wantApplied[i], resp.Results[i])
		}
	}
	if resp.Results[1].ErrorCode != "concern_waive_conflict" {
		t.Errorf("results[1].error_code = %q, want concern_waive_conflict", resp.Results[1].ErrorCode)
	}
	// The third item still applied: a mid-batch failure does NOT abort the rest.
	assertConcernStates(t, base, map[uuid.UUID]concern.State{
		a.ID: concern.StateWaived, b.ID: concern.StateRaised, c.ID: concern.StateWaived,
	})
	failed := auditEntriesByCategory(au, CategoryConcernWaiveFailed)
	if len(failed) != 1 {
		t.Fatalf("concern_waive_failed entries = %d, want 1", len(failed))
	}
	var payload map[string]any
	if err := json.Unmarshal(au.appended[failed[0]].Payload, &payload); err != nil {
		t.Fatalf("decode concern_waive_failed payload: %v", err)
	}
	if payload["concern_id"] != b.ID.String() {
		t.Errorf("concern_waive_failed names %v, want the failed id %s", payload["concern_id"], b.ID)
	}
}
