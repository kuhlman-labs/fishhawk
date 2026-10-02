package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The human-only server-check clearing guard (E80.3 / #3760). Every arm seeds
// a server_check row BY CONSTRUCTION through fakeConcernRepo.InsertRaised (never
// by calling the guard), then READS THE ROW — and the audit fake — back after
// the call returns, so a deleted call site surfaces as committed state (the row
// waived / deferred, a concern_waived entry appended) rather than as a matching
// error identity.
//
// COUNTERFACTUAL CONVENTION: each refusal arm names the call site whose
// deletion it observes and the fixture state that makes the deletion
// observable. The helper itself is never deleted (that stops compilation);
// either one call site is deleted, or refuseNonHumanServerCheckClear's body is
// mutated to `return false`, which reddens every refusal arm at once.

// serverCheckKey is a placeholder de-duplication key. It names a pattern class
// and a path only; no fixture in this file carries credential-shaped bytes.
const serverCheckKey = "diff_secrets|github-pat-classic|config/settings.go"

// seedServerCheckRow inserts one open server_check concern the way the diff
// secrets check raises it: empty reviewer model and role, provenance
// server_check, category security, with the given severity.
func seedServerCheckRow(t *testing.T, cr *fakeConcernRepo, runID, stageID uuid.UUID, severity string) *concern.Concern {
	t.Helper()
	rows, err := cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID:                runID,
		StageID:              stageID,
		StageKind:            concern.StageKindImplement,
		Provenance:           concern.ProvenanceServerCheck,
		OriginReviewSequence: 9,
		Concerns: []concern.RaisedConcern{{
			Severity: severity,
			Category: "security",
			Note:     "credential-shaped addition (github-pat-classic) at config/settings.go:2",
			CheckKey: serverCheckKey,
		}},
	})
	if err != nil {
		t.Fatalf("seed server_check concern: %v", err)
	}
	if !rows[0].IsServerCheck() || rows[0].CheckKey != serverCheckKey {
		t.Fatalf("seeded row provenance/check_key = %q/%q, want server_check/%q (fakeConcernRepo dropped a field)",
			rows[0].Provenance, rows[0].CheckKey, serverCheckKey)
	}
	return rows[0]
}

// postWaiveAs is postWaive with an explicit identity.
func postWaiveAs(t *testing.T, s *Server, concernID string, body waiveConcernRequest, auth func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+concernID+"/waive", bytes.NewReader(raw))
	req.SetPathValue("concern_id", concernID)
	w := httptest.NewRecorder()
	s.handleWaiveConcern(w, auth(req))
	return w
}

// withRunBoundAuth binds the request to a run-bound mcp:run:<runID> token.
func withRunBoundAuth(runID uuid.UUID) func(*http.Request) *http.Request {
	return func(req *http.Request) *http.Request { return withMCPFixupAuth(req, runID) }
}

// assertRequiresHuman checks the 403 concern_requires_human envelope and its
// details. A wrong status is reported with Errorf (not Fatalf) so the caller's
// committed-state read-backs still run and a deleted guard reports the row it
// let through, not only the status code.
func assertRequiresHuman(t *testing.T, w *httptest.ResponseRecorder, row *concern.Concern, verb, refusedActor string) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 concern_requires_human:\n%s", w.Code, w.Body.String())
		return
	}
	env := decodeErrorEnvelope(t, w)
	if env.Code != errCodeConcernRequiresHuman {
		t.Errorf("code = %q, want %q:\n%s", env.Code, errCodeConcernRequiresHuman, w.Body.String())
		return
	}
	for k, want := range map[string]string{
		"concern_id":    row.ID.String(),
		"provenance":    concern.ProvenanceServerCheck,
		"verb":          verb,
		"refused_actor": refusedActor,
	} {
		if got, _ := env.Details[k].(string); got != want {
			t.Errorf("details.%s = %q, want %q", k, got, want)
		}
	}
}

// assertState reads the row back from the store after the call returned.
func assertState(t *testing.T, cr *fakeConcernRepo, id uuid.UUID, want concern.State) {
	t.Helper()
	rows, err := cr.GetByIDs(context.Background(), []uuid.UUID{id})
	if err != nil {
		t.Fatalf("read back concern %s: %v", id, err)
	}
	if rows[0].State != want {
		t.Errorf("concern %s stored state = %q, want %q", id, rows[0].State, want)
	}
}

func assertNoWaivedEntries(t *testing.T, au *auditFake) {
	t.Helper()
	if n := len(auditEntriesByCategory(au, CategoryConcernWaived)); n != 0 {
		t.Errorf("concern_waived entries = %d, want 0 (the refusal precedes the intent append)", n)
	}
}

// TestServerCheckConcern_WaiveRefused_OperatorAgentToken: an operator-agent
// token (holding write:stages, not run-bound) cannot waive a server_check row.
//
// COUNTERFACTUAL (waive.go call deleted): this token passes the scope check and
// is not an mcp:run: subject, so no cross_run guard applies and the request is
// not delegated, so checkDelegation never runs — the deleted guard is the ONLY
// thing between it and applyConcernWaive. With it gone the row reads back
// WAIVED and one concern_waived entry exists → RED.
func TestServerCheckConcern_WaiveRefused_OperatorAgentToken(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedServerCheckRow(t, cr, uuid.New(), uuid.New(), "high")

	w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "agent says it is a fixture"}, withOperatorAgentAuth)

	assertRequiresHuman(t, w, row, clearVerbWaive, refusedActorAgentToken)
	assertState(t, cr, row.ID, concern.StateRaised)
	assertNoWaivedEntries(t, au)
}

// TestServerCheckConcern_WaiveRefused_RunBoundTokenOwnRun: a run-bound
// mcp:run: token naming the concern's OWN run cannot waive it.
//
// COUNTERFACTUAL (waive.go call deleted): the subject names the concern's own
// run id, so the cross_run_waive guard PASSES (TestWaiveConcern_MCPTokenOwnRunSucceeds
// is the existing proof the path is otherwise open) — the row reads back
// WAIVED → RED.
func TestServerCheckConcern_WaiveRefused_RunBoundTokenOwnRun(t *testing.T) {
	s, au, cr := waiveServer(t)
	runID := uuid.New()
	row := seedServerCheckRow(t, cr, runID, uuid.New(), "high")

	w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "own-run agent waive"}, withRunBoundAuth(runID))

	assertRequiresHuman(t, w, row, clearVerbWaive, refusedActorAgentToken)
	assertState(t, cr, row.ID, concern.StateRaised)
	assertNoWaivedEntries(t, au)
}

// TestServerCheckConcern_DelegatedWaiveRefused: a HUMAN subject sending
// delegated:true cannot waive a server_check row.
//
// COUNTERFACTUAL (waive.go call deleted): the fixture seeds the server_check
// row with severity LOW as the run's ONLY open concern and configures an
// operator_agent block delegating may_waive under solo_low, so with the guard
// gone checkDelegation PASSES and the read-back sees WAIVED → RED. Without that
// fixture (a high row, or no RunRepo) checkDelegation would refuse on its own
// and MASK the deletion.
func TestServerCheckConcern_DelegatedWaiveRefused(t *testing.T) {
	s, repo, au, cr := delegatedWaiveServer(t)
	runID := uuid.New()
	row := seedServerCheckRow(t, cr, runID, uuid.New(), "low")
	repo.seedRun(&run.Run{
		ID:           runID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: []byte(delegatedActionSpecYAML),
	})

	w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "solo low, delegated", Delegated: true}, withAuth)

	assertRequiresHuman(t, w, row, clearVerbWaive, refusedActorDelegated)
	assertState(t, cr, row.ID, concern.StateRaised)
	assertNoWaivedEntries(t, au)
}

// TestServerCheckConcern_HumanWaiveSucceeds: the clearing path the guard leaves
// open — a human, non-delegated waive with a reason — succeeds, and the
// concern_waived payload carries provenance server_check so the chain records
// that a human cleared a server check.
//
// COUNTERFACTUAL (the provenance stamp in applyConcernWaive deleted): the
// payload carries no provenance key → RED.
func TestServerCheckConcern_HumanWaiveSucceeds(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedServerCheckRow(t, cr, uuid.New(), uuid.New(), "high")

	w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "known test fixture"}, withAuth)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertState(t, cr, row.ID, concern.StateWaived)
	payload := singleWaivedPayload(t, au)
	if payload["provenance"] != concern.ProvenanceServerCheck {
		t.Errorf("concern_waived payload provenance = %v, want %q", payload["provenance"], concern.ProvenanceServerCheck)
	}
}

// TestServerCheckConcern_ReviewerConcernUnaffected: the guard is
// provenance-scoped — the same operator-agent token that is refused on a
// server_check row waives a reviewer-raised (provenance empty) concern, and
// that waive's payload carries NO provenance key (byte-identical to before).
//
// COUNTERFACTUAL (guard body mutated to drop the IsServerCheck test, i.e. refuse
// every agent): this arm reads 403 → RED. Stamping provenance unconditionally
// reddens the no-provenance-key assertion.
func TestServerCheckConcern_ReviewerConcernUnaffected(t *testing.T) {
	s, au, cr := waiveServer(t)
	row := seedConcernRow(t, cr, uuid.New(), uuid.New(), concern.StageKindImplement, 100, "naming nit")

	w := postWaiveAs(t, s, row.ID.String(), waiveConcernRequest{Reason: "accepted trade-off"}, withOperatorAgentAuth)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a reviewer concern is not human-only):\n%s", w.Code, w.Body.String())
	}
	assertState(t, cr, row.ID, concern.StateWaived)
	if _, ok := singleWaivedPayload(t, au)["provenance"]; ok {
		t.Errorf("reviewer-concern concern_waived payload carries a provenance key; want it absent (byte-identical payload)")
	}
}

func singleWaivedPayload(t *testing.T, au *auditFake) map[string]any {
	t.Helper()
	idx := auditEntriesByCategory(au, CategoryConcernWaived)
	if len(idx) != 1 {
		t.Fatalf("concern_waived entries = %d, want 1", len(idx))
	}
	au.mu.Lock()
	raw := au.appended[idx[0]].Payload
	au.mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode concern_waived payload: %v", err)
	}
	return payload
}

// TestServerCheckConcern_DeferRefused_AgentToken: neither an operator-agent
// token nor an own-run mcp:run: token can defer a server_check row, and NO work
// item is filed.
//
// COUNTERFACTUAL (defer_concern.go call deleted): both identities hold a write
// scope, the own-run token passes the cross_run_defer guard, the row is open,
// the request names a parent epic and child number (a complete filing), and
// deferServer wires a working provider — so the provider's File is called and
// the row reads back DEFERRED → RED on both arms.
func TestServerCheckConcern_DeferRefused_AgentToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth func(runID uuid.UUID) func(*http.Request) *http.Request
	}{
		{"operator_agent", func(uuid.UUID) func(*http.Request) *http.Request { return withOperatorAgentAuth }},
		{"run_bound_own_run", withRunBoundAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au, cr, fp := deferServer(t)
			runID := uuid.New()
			seedDeferRun(repo, runID)
			row := seedServerCheckRow(t, cr, runID, uuid.New(), "high")

			// A COMPLETE filing request (parent epic + child number): an empty
			// one fails filing validation with 422 downstream of the guard, which
			// would MASK a deleted call site behind a non-403 that files nothing.
			w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "3"}, tc.auth(runID))

			// Committed state FIRST, so a deleted guard reports the filed work
			// item and the DEFERRED row, not only the status code.
			if fp.called {
				t.Errorf("provider.File was called; a refused defer must file no work item")
			}
			assertState(t, cr, row.ID, concern.StateRaised)
			if n := len(auditEntriesByCategory(au, CategoryConcernDeferred)); n != 0 {
				t.Errorf("concern_deferred entries = %d, want 0", n)
			}
			assertRequiresHuman(t, w, row, clearVerbDefer, refusedActorAgentToken)
		})
	}
}

// TestServerCheckConcern_HumanDeferSucceeds: a human may defer a server_check
// row — the guard is not a blanket defer refusal.
func TestServerCheckConcern_HumanDeferSucceeds(t *testing.T) {
	s, repo, _, cr, fp := deferServer(t)
	runID := uuid.New()
	seedDeferRun(repo, runID)
	row := seedServerCheckRow(t, cr, runID, uuid.New(), "high")

	w := postDefer(t, s, row.ID.String(), deferConcernRequest{ParentEpic: "#389", N: "3"}, withAuth)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if !fp.called {
		t.Errorf("provider.File was not called on a human defer")
	}
	assertState(t, cr, row.ID, concern.StateDeferred)
}

// TestServerCheckConcern_BulkWaiveRefusesWholeBatch: an agent bulk waive whose
// batch carries ONE server_check row refuses the WHOLE batch — the reviewer row
// listed FIRST (and already validated) is not waived either.
//
// COUNTERFACTUAL (bulk_waive.go call deleted): both identities hold a write
// scope, the own-run token names the path run, both rows are open and
// non-delegated, so nothing else refuses — both rows read back WAIVED → RED.
func TestServerCheckConcern_BulkWaiveRefusesWholeBatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth func(runID uuid.UUID) func(*http.Request) *http.Request
	}{
		{"operator_agent", func(uuid.UUID) func(*http.Request) *http.Request { return withOperatorAgentAuth }},
		{"run_bound_own_run", withRunBoundAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, au, cr := bulkWaiveServer(t)
			runID := uuid.New()
			reviewer := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "naming nit")
			check := seedServerCheckRow(t, cr, runID, uuid.New(), "high")

			w := postBulkWaiveAs(t, s, runID.String(), bulkWaiveRequest{
				ConcernIDs: []string{reviewer.ID.String(), check.ID.String()},
				Reason:     "batch clear",
			}, tc.auth(runID))

			assertRequiresHuman(t, w, check, clearVerbBulkWaive, refusedActorAgentToken)
			assertConcernStates(t, cr, map[uuid.UUID]concern.State{
				reviewer.ID: concern.StateRaised,
				check.ID:    concern.StateRaised,
			})
			assertNoWaivedEntries(t, au)
		})
	}
}

// TestServerCheckConcern_BulkWaiveRefusesDelegated: a HUMAN subject sending a
// delegated:true bulk waive naming a server_check row is refused.
//
// COUNTERFACTUAL (bulk_waive.go call deleted): the server_check row is LOW and
// the run's ONLY open concern under a may_waive: solo_low operator_agent block,
// so checkDelegation PASSES and the row reads back WAIVED → RED. A high row or
// a second open concern would make checkDelegation refuse and MASK the deletion.
func TestServerCheckConcern_BulkWaiveRefusesDelegated(t *testing.T) {
	repo := newApprovalRunRepo()
	au := newAuditFake()
	cr := newFakeConcernRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr})
	runID := uuid.New()
	row := seedServerCheckRow(t, cr, runID, uuid.New(), "low")
	repo.seedRun(&run.Run{
		ID:           runID,
		State:        run.StateRunning,
		WorkflowID:   "feature_change",
		WorkflowSpec: []byte(delegatedActionSpecYAML),
	})

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{row.ID.String()}, Reason: "solo low, delegated", Delegated: true,
	})

	assertRequiresHuman(t, w, row, clearVerbBulkWaive, refusedActorDelegated)
	assertState(t, cr, row.ID, concern.StateRaised)
	assertNoWaivedEntries(t, au)
}

// TestServerCheckConcern_HumanBulkWaiveSucceeds: a human, non-delegated bulk
// waive clears a server_check row alongside a reviewer row.
func TestServerCheckConcern_HumanBulkWaiveSucceeds(t *testing.T) {
	s, _, cr := bulkWaiveServer(t)
	runID := uuid.New()
	reviewer := seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, 1, "naming nit")
	check := seedServerCheckRow(t, cr, runID, uuid.New(), "high")

	w := postBulkWaive(t, s, runID.String(), bulkWaiveRequest{
		ConcernIDs: []string{reviewer.ID.String(), check.ID.String()},
		Reason:     "known test fixture",
	})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	assertConcernStates(t, cr, map[uuid.UUID]concern.State{
		reviewer.ID: concern.StateWaived,
		check.ID:    concern.StateWaived,
	})
}

// TestIsAgentSubject pins the ONE agent classification the clearing guard and
// the captain verbs share, and that captainActorIsAgent delegates to it.
func TestIsAgentSubject(t *testing.T) {
	cases := []struct {
		subject string
		want    bool
	}{
		{operatorAgentSubject, true},
		{"operator-agent/campaign", true},
		{"mcp:run:" + uuid.NewString(), true},
		{"github:alice", false},
		{"service:deployer", false},
		{"token:reader", false},
		{"anonymous", false},
		{"", false},
		{"operator-agent", false}, // no slash: not the token family
		{"mcp:runner", false},     // not the run-bound prefix
	}
	for _, tc := range cases {
		if got := isAgentSubject(tc.subject); got != tc.want {
			t.Errorf("isAgentSubject(%q) = %v, want %v", tc.subject, got, tc.want)
		}
		if got := captainActorIsAgent(tc.subject); got != tc.want {
			t.Errorf("captainActorIsAgent(%q) = %v, want %v (must agree with isAgentSubject)", tc.subject, got, tc.want)
		}
	}
}

// TestRefuseNonHumanServerCheckClear_NilAndHumanPassThrough pins the guard's
// two pass-through branches directly: a nil row and a human non-delegated
// subject on a server_check row both return false and write nothing.
func TestRefuseNonHumanServerCheckClear_NilAndHumanPassThrough(t *testing.T) {
	s, _, cr := waiveServer(t)
	row := seedServerCheckRow(t, cr, uuid.New(), uuid.New(), "high")
	req := httptest.NewRequest(http.MethodPost, "/", nil)

	for name, tc := range map[string]struct {
		row     *concern.Concern
		subject string
	}{
		"nil_row":           {nil, operatorAgentSubject},
		"human_on_check":    {row, "github:alice"},
		"anonymous_subject": {row, ""},
	} {
		w := httptest.NewRecorder()
		if s.refuseNonHumanServerCheckClear(w, req, tc.row, tc.subject, false, clearVerbWaive) {
			t.Errorf("%s: refused, want pass-through", name)
		}
		if w.Body.Len() != 0 {
			t.Errorf("%s: wrote a response body on pass-through: %s", name, w.Body.String())
		}
	}
}
