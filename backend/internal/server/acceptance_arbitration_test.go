package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// acceptance_arbitration_test.go pins POST
// /v0/runs/{run_id}/acceptance-arbitration (E66.37 / #2474): the operator-only
// discharge of a PAGED acceptance triage. One behavioral test per enumerated
// guard branch (the #1199 rule), plus the write-side revalidation of binding
// approval condition 1.
//
// Every REFUSAL case asserts ZERO acceptance_triage_arbitrated entries after the
// call in addition to the error identity. That is load-bearing for the
// counterfactual pass: each guard's real effect is COMMITTED STATE, and a guard
// that refuses AFTER writing would return a byte-identical error body, so an
// error-code-only assertion would stay green with the guard deleted.

const (
	// arbitrationOutcomeSeq is the acceptance_outcome_recorded sequence the
	// fixtures bind to; the correlated triage decision sits one above it (the
	// backend writes the triage AFTER the outcome it triages).
	arbitrationOutcomeSeq = int64(60)
	arbitrationTriageSeq  = int64(61)
)

// seedAcceptanceOutcomeDetail seeds one acceptance_outcome_recorded entry with
// the full payload the arbitration endpoint reads: verdict + the per-result
// counts + the stage the outcome was scoped to.
func seedAcceptanceOutcomeDetail(au *auditFake, runID, stageID uuid.UUID, seq int64,
	verdict string, criteriaFailed, criteriaSkipped int) {
	rid, sid := runID, stageID
	p, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"stage_id":         stageID.String(),
		"verdict":          verdict,
		"criteria_failed":  criteriaFailed,
		"criteria_skipped": criteriaSkipped,
	})
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryAcceptanceOutcomeRecorded,
		Sequence: seq, Payload: p,
	})
	au.mu.Unlock()
}

// seedAcceptanceTriageDecision seeds one acceptance_triage_decided entry.
func seedAcceptanceTriageDecision(au *auditFake, runID, stageID uuid.UUID, seq int64, class, disposition string) {
	rid, sid := runID, stageID
	p, _ := json.Marshal(map[string]any{
		"class":       class,
		"disposition": disposition,
	})
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryAcceptanceTriageDecided,
		Sequence: seq, Payload: p,
	})
	au.mu.Unlock()
}

// seedAcceptanceArbitrationEntry seeds a pre-existing acceptance_triage_arbitrated
// entry naming outcomeSequence — used for the idempotence and
// already-arbitrated-gate cases.
func seedAcceptanceArbitrationEntry(au *auditFake, runID uuid.UUID, seq, outcomeSequence int64) {
	rid := runID
	p, _ := json.Marshal(map[string]any{
		"run_id":           runID.String(),
		"reason":           "seeded prior arbitration",
		"outcome_sequence": outcomeSequence,
	})
	au.mu.Lock()
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, Category: CategoryAcceptanceTriageArbitrated,
		Sequence: seq, Payload: p,
	})
	au.mu.Unlock()
}

// arbitrationRows returns every APPENDED acceptance_triage_arbitrated param.
// Seeded fixtures are deliberately excluded: the refusal assertions ask "did
// this call write?", not "does the chain carry one?".
func arbitrationRows(au *auditFake) []audit.ChainAppendParams {
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []audit.ChainAppendParams
	for i := range au.appended {
		if au.appended[i].Category == CategoryAcceptanceTriageArbitrated {
			out = append(out, au.appended[i])
		}
	}
	return out
}

// postArbitration posts an arbitration body with the given identity mutator.
func postArbitration(t *testing.T, s *Server, runID uuid.UUID, body acceptanceArbitrationRequest,
	withID func(*http.Request) *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/acceptance-arbitration", bytes.NewReader(raw))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleAcceptanceArbitration(w, withID(req))
	return w
}

// seedPagedTriageRun wires the canonical arbitrable state: a run whose workflow
// declares an acceptance stage, a terminal acceptance stage, a FAILED verdict at
// arbitrationOutcomeSeq carrying the given failed/skipped counts, and a
// CORRELATED triage decision (sequence above the outcome) with the given
// disposition. Returns the acceptance stage id.
func seedPagedTriageRun(t *testing.T, repo *autoDriveRepo, au *auditFake, runID uuid.UUID,
	class, disposition string, criteriaFailed, criteriaSkipped int) uuid.UUID {
	t.Helper()
	stages := acceptanceMergeStages(runID, run.StageStateSucceeded)
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, []byte(autoDriveAcceptanceSpecYAML), stages)
	accID := stages[2].ID
	seedAcceptanceOutcomeDetail(au, runID, accID, arbitrationOutcomeSeq,
		acceptanceVerdictFailed, criteriaFailed, criteriaSkipped)
	seedAcceptanceTriageDecision(au, runID, accID, arbitrationTriageSeq, class, disposition)
	return accID
}

// class5Reason is the operator prose the all-skip fixtures pass.
const class5Reason = "every criterion is externally unvalidatable in the default-deny sandbox; shipping and tracking the walk separately"

// --- auth ladder ------------------------------------------------------------

func TestAcceptanceArbitration_AnonymousUnauthorized(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason},
		func(req *http.Request) *http.Request { return req })
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("authentication_required")) {
		t.Errorf("body missing authentication_required: %s", w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written despite an anonymous caller")
	}
}

// TestAcceptanceArbitration_RunBoundTokenForbidden: a run-bound agent token for
// THIS run, even carrying write:approvals, may not discharge its own acceptance
// failure — the arbitration admits the same merge fishhawk_merge_run does.
func TestAcceptanceArbitration_RunBoundTokenForbidden(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	withOwnRunToken := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "mcp:run:" + runID.String(),
			TokenID: "tok-agent",
			Scopes:  []string{"mcp:read", "write:approvals"},
		}))
	}
	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withOwnRunToken)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_token_forbidden")) {
		t.Errorf("body missing run_token_forbidden: %s", w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written despite a run-bound agent token")
	}
}

// TestAcceptanceArbitration_MissingApprovalsScopeForbidden: write:approvals is
// enforced UNCONDITIONALLY — a cookie-session identity (empty TokenID, no
// scopes) is refused too, not waved through.
func TestAcceptanceArbitration_MissingApprovalsScopeForbidden(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	withUnscoped := func(req *http.Request) *http.Request {
		return req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
			Subject: "github:ops", Scopes: []string{"read:runs"},
		}))
	}
	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withUnscoped)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("insufficient_scope")) {
		t.Errorf("body missing insufficient_scope: %s", w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written despite a caller without write:approvals")
	}
}

// --- request-shape guards ---------------------------------------------------

func TestAcceptanceArbitration_EmptyReasonRejected(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: "   "}, withMergeOperator)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("validation_failed")) {
		t.Errorf("body missing validation_failed: %s", w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written despite a whitespace-only reason")
	}
}

func TestAcceptanceArbitration_InvalidRunID(t *testing.T) {
	s, _, au := newAutoDriveMergeServer(t, &fakeMerger{})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/acceptance-arbitration",
		bytes.NewReader([]byte(`{"reason":"x"}`)))
	req.SetPathValue("run_id", "not-a-uuid")
	w := httptest.NewRecorder()
	s.handleAcceptanceArbitration(w, withMergeOperator(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written despite a non-UUID run_id")
	}
}

func TestAcceptanceArbitration_MalformedBody(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	req := httptest.NewRequest(http.MethodPost,
		"/v0/runs/"+runID.String()+"/acceptance-arbitration", bytes.NewReader([]byte("{not json")))
	req.SetPathValue("run_id", runID.String())
	w := httptest.NewRecorder()
	s.handleAcceptanceArbitration(w, withMergeOperator(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written despite a malformed body")
	}
}

func TestAcceptanceArbitration_UnconfiguredRepos(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := postArbitration(t, s, uuid.New(), acceptanceArbitrationRequest{Reason: "x"}, withMergeOperator)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_arbitration_unconfigured")) {
		t.Errorf("body missing acceptance_arbitration_unconfigured: %s", w.Body.String())
	}
}

func TestAcceptanceArbitration_RunNotFound(t *testing.T) {
	s, _, au := newAutoDriveMergeServer(t, &fakeMerger{})
	w := postArbitration(t, s, uuid.New(), acceptanceArbitrationRequest{Reason: "x"}, withMergeOperator)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("run_not_found")) {
		t.Errorf("body missing run_not_found: %s", w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written for an unknown run")
	}
}

// --- gate-state precondition ------------------------------------------------

// TestAcceptanceArbitration_GateNotTriage: only a run PARKED at
// acceptance_triage has something to discharge. Every other gate state — a pass,
// a still-pending stage, a workflow with no acceptance stage at all, and a run
// already discharged — is refused, so the verb can never manufacture a
// merge-eligible state out of thin air.
func TestAcceptanceArbitration_GateNotTriage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, repo *autoDriveRepo, au *auditFake, runID uuid.UUID)
		state string
	}{
		{
			name: "passed verdict",
			seed: func(t *testing.T, repo *autoDriveRepo, au *auditFake, runID uuid.UUID) {
				stages := acceptanceMergeStages(runID, run.StageStateSucceeded)
				seedMergeRun(t, repo, runID, run.StateRunning, mergePR, []byte(autoDriveAcceptanceSpecYAML), stages)
				seedAcceptanceOutcomeDetail(au, runID, stages[2].ID, arbitrationOutcomeSeq, acceptanceVerdictPassed, 0, 0)
			},
			state: acceptanceGatePassed,
		},
		{
			name: "acceptance still pending",
			seed: func(t *testing.T, repo *autoDriveRepo, au *auditFake, runID uuid.UUID) {
				seedMergeRun(t, repo, runID, run.StateRunning, mergePR,
					[]byte(autoDriveAcceptanceSpecYAML), acceptanceMergeStages(runID, run.StageStateRunning))
			},
			state: acceptanceGatePending,
		},
		{
			name: "no acceptance stage declared",
			seed: func(t *testing.T, repo *autoDriveRepo, au *auditFake, runID uuid.UUID) {
				seedMergeRun(t, repo, runID, run.StateRunning, mergePR, nil, nil)
			},
			state: acceptanceGateNotDeclared,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
			runID := uuid.New()
			tc.seed(t, repo, au, runID)

			w := postArbitration(t, s, runID,
				acceptanceArbitrationRequest{Reason: class5Reason, AcknowledgeFailedCriteria: true}, withMergeOperator)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_arbitration_not_applicable")) {
				t.Errorf("body missing acceptance_arbitration_not_applicable: %s", w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte(`"acceptance_gate_state":"`+tc.state+`"`)) {
				t.Errorf("body missing acceptance_gate_state %q: %s", tc.state, w.Body.String())
			}
			if len(arbitrationRows(au)) != 0 {
				t.Errorf("arbitration written on a %s gate", tc.state)
			}
		})
	}
}

// TestAcceptanceArbitration_GateReadErrorFailsClosed: an unreadable audit chain
// is a 500 and NEVER a write — the verb must not record a discharge on evidence
// it could not read.
func TestAcceptanceArbitration_GateReadErrorFailsClosed(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)
	au.listByCategoryErr = errBoom

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written despite an acceptance-gate read error")
	}
}

// --- triage-disposition precondition ----------------------------------------

// TestAcceptanceArbitration_AutoRoutedDispositionRefused is the issue's
// class-1/2 criterion: a verdict the deterministic triage AUTO-ROUTED keeps its
// automatic route. Arbitration is not a way to skip a fix-up the loop already
// dispatched.
func TestAcceptanceArbitration_AutoRoutedDispositionRefused(t *testing.T) {
	for _, disposition := range []string{
		acceptanceDispositionFixupDispatched,
		acceptanceDispositionRetryDispatched,
	} {
		t.Run(disposition, func(t *testing.T) {
			s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
			runID := uuid.New()
			seedPagedTriageRun(t, repo, au, runID, acceptanceClass1, disposition, 1, 0)

			w := postArbitration(t, s, runID,
				acceptanceArbitrationRequest{Reason: "ship it", AcknowledgeFailedCriteria: true}, withMergeOperator)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_arbitration_not_applicable")) {
				t.Errorf("body missing acceptance_arbitration_not_applicable: %s", w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte(`"triage_disposition":"`+disposition+`"`)) {
				t.Errorf("body missing triage_disposition %q: %s", disposition, w.Body.String())
			}
			if len(arbitrationRows(au)) != 0 {
				t.Errorf("arbitration written on an auto-routed %s disposition", disposition)
			}
		})
	}
}

// TestAcceptanceArbitration_NoCorrelatedTriageRefused: the triage decision must
// be NEWER than the outcome it triages. A triage entry at or BELOW the outcome's
// sequence belongs to an earlier attempt, so it does not correlate and the verb
// refuses. This is also the ordering canary named in the plan's risk list: if
// handleShipAcceptance ever wrote the triage BEFORE the outcome, this case would
// become the production shape and fail here.
func TestAcceptanceArbitration_NoCorrelatedTriageRefused(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	stages := acceptanceMergeStages(runID, run.StageStateSucceeded)
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, []byte(autoDriveAcceptanceSpecYAML), stages)
	seedAcceptanceOutcomeDetail(au, runID, stages[2].ID, arbitrationOutcomeSeq, acceptanceVerdictFailed, 0, 3)
	// A PAGED triage decision, but from an EARLIER attempt (below the outcome).
	seedAcceptanceTriageDecision(au, runID, stages[2].ID, arbitrationOutcomeSeq-10,
		acceptanceClass5, acceptanceDispositionUnvalidatable)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_arbitration_not_applicable")) {
		t.Errorf("body missing acceptance_arbitration_not_applicable: %s", w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written with no triage decision correlated to this outcome")
	}
}

// TestAcceptanceArbitration_UndecodableDispositionRefused: an unreadable triage
// payload yields an empty disposition, which acceptanceDispositionPages refuses
// — fail-closed on unknown evidence rather than assuming it paged.
func TestAcceptanceArbitration_UndecodableDispositionRefused(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	stages := acceptanceMergeStages(runID, run.StageStateSucceeded)
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, []byte(autoDriveAcceptanceSpecYAML), stages)
	seedAcceptanceOutcomeDetail(au, runID, stages[2].ID, arbitrationOutcomeSeq, acceptanceVerdictFailed, 0, 3)
	rid, sid := runID, stages[2].ID
	au.seeded = append(au.seeded, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryAcceptanceTriageDecided,
		Sequence: arbitrationTriageSeq, Payload: []byte(`not json`),
	})

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written on an undecodable triage disposition")
	}
}

// --- failed-criteria acknowledgement ----------------------------------------

// TestAcceptanceArbitration_FailedCriteriaRequireAcknowledgement: a verdict with
// genuinely FAILED criteria needs the operator's separately-stated decision. The
// reason alone is not it.
func TestAcceptanceArbitration_FailedCriteriaRequireAcknowledgement(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass1, acceptanceDispositionFixupUnavailable, 1, 0)

	w := postArbitration(t, s, runID,
		acceptanceArbitrationRequest{Reason: "the failing criterion went stale mid-run"}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_arbitration_requires_acknowledgement")) {
		t.Errorf("body missing acceptance_arbitration_requires_acknowledgement: %s", w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"criteria_failed":1`)) {
		t.Errorf("body missing criteria_failed detail: %s", w.Body.String())
	}
	if len(arbitrationRows(au)) != 0 {
		t.Error("arbitration written for failed criteria without the acknowledgement")
	}
}

// TestAcceptanceArbitration_FailedCriteriaAcknowledgedSucceeds is the wider
// reading the operator confirmed (binding condition 6): a class-1
// fixup_unavailable_paged verdict PAGED, so it is arbitrable — with the explicit
// acknowledgement — even though its class number is 1.
func TestAcceptanceArbitration_FailedCriteriaAcknowledgedSucceeds(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass1, acceptanceDispositionFixupUnavailable, 1, 0)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{
		Reason:                    "the failing criterion went stale mid-run; 6 of 7 passed",
		AcknowledgeFailedCriteria: true,
	}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(arbitrationRows(au)) != 1 {
		t.Fatalf("arbitration rows = %d, want 1", len(arbitrationRows(au)))
	}
}

// TestAcceptanceArbitration_AllSkipSucceedsWithReasonAlone is the issue's
// class-5 wedge case: nothing FAILED (every criterion was skipped as externally
// unvalidatable), so the reason alone discharges it — no acknowledgement of
// failed criteria is demanded for criteria that never failed.
func TestAcceptanceArbitration_AllSkipSucceedsWithReasonAlone(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	accID := seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceArbitrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.AcceptanceGateState != acceptanceGateArbitrated {
		t.Errorf("acceptance_gate_state = %q, want %q", resp.AcceptanceGateState, acceptanceGateArbitrated)
	}
	if resp.OutcomeSequence != arbitrationOutcomeSeq {
		t.Errorf("outcome_sequence = %d, want %d", resp.OutcomeSequence, arbitrationOutcomeSeq)
	}
	if resp.AlreadyRecorded {
		t.Error("already_recorded = true on a first POST, want false")
	}
	rows := arbitrationRows(au)
	if len(rows) != 1 {
		t.Fatalf("arbitration rows = %d, want 1", len(rows))
	}
	if rows[0].StageID == nil || *rows[0].StageID != accID {
		t.Errorf("stage_id = %v, want the acceptance stage %s", rows[0].StageID, accID)
	}
}

// TestAcceptanceArbitration_PayloadShape pins every field the recorded
// declaration carries — the sequence binding, the operator's reason, the
// acknowledgement flag, the triage class/disposition context, and delegated:false.
func TestAcceptanceArbitration_PayloadShape(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	accID := seedPagedTriageRun(t, repo, au, runID, acceptanceClass1, acceptanceDispositionFixupUnavailable, 2, 1)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{
		Reason: "shipping despite 2 failed criteria", AcknowledgeFailedCriteria: true,
	}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows := arbitrationRows(au)
	if len(rows) != 1 {
		t.Fatalf("arbitration rows = %d, want 1", len(rows))
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorUser {
		t.Errorf("actor kind = %v, want user", rows[0].ActorKind)
	}
	if rows[0].ActorSubject == nil || *rows[0].ActorSubject != "github:ops" {
		t.Errorf("actor subject = %v, want github:ops", rows[0].ActorSubject)
	}
	var p struct {
		RunID           string `json:"run_id"`
		StageID         string `json:"stage_id"`
		Reason          string `json:"reason"`
		OutcomeSequence int64  `json:"outcome_sequence"`
		Verdict         string `json:"verdict"`
		CriteriaFailed  int    `json:"criteria_failed"`
		CriteriaSkipped int    `json:"criteria_skipped"`
		Acknowledged    bool   `json:"acknowledged_failed_criteria"`
		TriageClass     string `json:"triage_class"`
		Disposition     string `json:"triage_disposition"`
		Delegated       bool   `json:"delegated"`
	}
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.RunID != runID.String() || p.StageID != accID.String() {
		t.Errorf("run_id/stage_id = %q/%q, want %s/%s", p.RunID, p.StageID, runID, accID)
	}
	if p.Reason != "shipping despite 2 failed criteria" {
		t.Errorf("reason = %q", p.Reason)
	}
	if p.OutcomeSequence != arbitrationOutcomeSeq {
		t.Errorf("outcome_sequence = %d, want %d (the binding this whole feature turns on)", p.OutcomeSequence, arbitrationOutcomeSeq)
	}
	if p.Verdict != acceptanceVerdictFailed || p.CriteriaFailed != 2 || p.CriteriaSkipped != 1 {
		t.Errorf("verdict/failed/skipped = %q/%d/%d, want failed/2/1", p.Verdict, p.CriteriaFailed, p.CriteriaSkipped)
	}
	if !p.Acknowledged {
		t.Error("acknowledged_failed_criteria = false, want true")
	}
	if p.TriageClass != acceptanceClass1 || p.Disposition != acceptanceDispositionFixupUnavailable {
		t.Errorf("triage class/disposition = %q/%q", p.TriageClass, p.Disposition)
	}
	if p.Delegated {
		t.Error("delegated = true, want false (this is the operator path)")
	}
}

// TestAcceptanceArbitration_Idempotent: a POST that finds a PRIOR arbitration
// already bound to the newest outcome appends no second row and reports
// already_recorded with that row's sequence. Seeded by construction (a prior
// arbitration entry) so the RED under deletion of the short-circuit lands on the
// duplicate-row assertion, not on fixture setup.
func TestAcceptanceArbitration_Idempotent(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)
	seedAcceptanceArbitrationEntry(au, runID, arbitrationTriageSeq+1, arbitrationOutcomeSeq)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceArbitrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AlreadyRecorded {
		t.Error("already_recorded = false, want true on a re-POST for the same outcome")
	}
	if resp.ArbitrationSequence != arbitrationTriageSeq+1 {
		t.Errorf("arbitration_sequence = %d, want the existing row's %d", resp.ArbitrationSequence, arbitrationTriageSeq+1)
	}
	if rows := arbitrationRows(au); len(rows) != 0 {
		t.Fatalf("appended arbitration rows = %d, want 0 (the prior row must be reused, never duplicated)", len(rows))
	}
}

// TestAcceptanceArbitration_IdempotentRePost drives the endpoint TWICE against
// one mutable audit fake — the real shape an operator hits after a timed-out
// first call. The second POST must reuse the first call's row: exactly one
// acceptance_triage_arbitrated entry, ever.
func TestAcceptanceArbitration_IdempotentRePost(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	first := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200:\n%s", first.Code, first.Body.String())
	}
	second := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (idempotent):\n%s", second.Code, second.Body.String())
	}
	var resp acceptanceArbitrationResponse
	if err := json.Unmarshal(second.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.AlreadyRecorded {
		t.Error("second POST already_recorded = false, want true")
	}
	if rows := arbitrationRows(au); len(rows) != 1 {
		t.Fatalf("arbitration rows after two POSTs = %d, want 1 (no duplicate)", len(rows))
	}
}

// TestAcceptanceArbitration_ArbitrationForOlderOutcomeDoesNotShortCircuit: the
// idempotence check is bound to the outcome_sequence, not to the mere presence
// of a prior arbitration. A stale arbitration naming an EARLIER outcome must not
// short-circuit a fresh discharge of the current one.
func TestAcceptanceArbitration_ArbitrationForOlderOutcomeDoesNotShortCircuit(t *testing.T) {
	s, repo, au := newAutoDriveMergeServer(t, &fakeMerger{})
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)
	seedAcceptanceArbitrationEntry(au, runID, arbitrationOutcomeSeq-5, arbitrationOutcomeSeq-20)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp acceptanceArbitrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.AlreadyRecorded {
		t.Error("already_recorded = true — a stale arbitration naming an OLDER outcome must not satisfy this one")
	}
	if rows := arbitrationRows(au); len(rows) != 1 {
		t.Fatalf("arbitration rows = %d, want 1 (a fresh discharge was required)", len(rows))
	}
}

// --- write-side revalidation (binding approval condition 1) -----------------

// supersedingAuditRepo makes the acceptance outcome MOVE mid-request: after
// `afterCalls` reads of the acceptance_outcome_recorded category it starts
// returning an additional, NEWER outcome entry. That models a concurrent
// acceptance re-run (or the delegated auto-driver's own path) landing between
// the endpoint's guard evaluation and its append — the interleaving binding
// condition 1 exists to close.
//
// The window is seeded BY CONSTRUCTION rather than by calling the control: the
// fake mutates on its own read counter, so the RED under deletion lands on the
// behavioral assertion (a persisted arbitration naming a superseded outcome),
// not on fixture setup.
type supersedingAuditRepo struct {
	*auditFake
	mu         sync.Mutex
	afterCalls int
	calls      int
	newerSeq   int64
	runID      uuid.UUID
	stageID    uuid.UUID
}

func (r *supersedingAuditRepo) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	out, err := r.auditFake.ListForRunByCategory(ctx, runID, category)
	if err != nil || category != CategoryAcceptanceOutcomeRecorded {
		return out, err
	}
	r.mu.Lock()
	r.calls++
	fire := r.calls > r.afterCalls
	r.mu.Unlock()
	if !fire {
		return out, nil
	}
	rid, sid := r.runID, r.stageID
	p, _ := json.Marshal(map[string]any{
		"verdict": acceptanceVerdictFailed, "criteria_failed": 0, "criteria_skipped": 3,
	})
	return append(out, &audit.Entry{
		RunID: &rid, StageID: &sid, Category: CategoryAcceptanceOutcomeRecorded,
		Sequence: r.newerSeq, Payload: p,
	}), nil
}

// TestAcceptanceArbitration_OutcomeSupersededBeforeAppend is binding approval
// condition 1 + condition 4(c): a concurrent acceptance re-run supersedes the
// outcome AFTER every guard passed and BEFORE the append. The endpoint must
// refuse 409 naming the changed sequence and leave ZERO arbitration rows —
// without the revalidation it would persist an operator discharge of a verdict
// nobody evaluated, which is also the precondition that makes the read-side
// defect reachable.
func TestAcceptanceArbitration_OutcomeSupersededBeforeAppend(t *testing.T) {
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	runID := uuid.New()
	stages := acceptanceMergeStages(runID, run.StageStateSucceeded)
	sup := &supersedingAuditRepo{
		auditFake: au,
		// Calls 1 (the gate's read) and 2 (the guard's outcome read) see the
		// ORIGINAL outcome; the revalidation read immediately before the append
		// sees the superseding one.
		afterCalls: 2,
		newerSeq:   arbitrationOutcomeSeq + 40,
		runID:      runID,
		stageID:    stages[2].ID,
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: sup, GateMerger: &fakeMerger{}})
	seedMergeRun(t, repo, runID, run.StateRunning, mergePR, []byte(autoDriveAcceptanceSpecYAML), stages)
	seedAcceptanceOutcomeDetail(au, runID, stages[2].ID, arbitrationOutcomeSeq, acceptanceVerdictFailed, 0, 3)
	seedAcceptanceTriageDecision(au, runID, stages[2].ID, arbitrationTriageSeq,
		acceptanceClass5, acceptanceDispositionUnvalidatable)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 acceptance_outcome_superseded:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_outcome_superseded")) {
		t.Errorf("body missing acceptance_outcome_superseded: %s", w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"current_outcome_sequence":100`)) {
		t.Errorf("body must name the CHANGED sequence: %s", w.Body.String())
	}
	// The load-bearing assertion: the control's real effect is committed state.
	if rows := arbitrationRows(au); len(rows) != 0 {
		t.Fatalf("arbitration rows = %d, want 0 — an arbitration naming superseded outcome %d was persisted",
			len(rows), arbitrationOutcomeSeq)
	}
}

// --- capability path (audit.AnchoredChainAppender, #2536) --------------------
//
// The atomic append (anchor re-read + dedupe scan under the run-row lock) lives
// in the store, so its BEHAVIOUR is pinned by the real-Postgres suite in
// acceptance_arbitration_pg_test.go. What these fake-backed cases pin is the
// HANDLER's mapping of the primitive's two typed errors onto the UNCHANGED wire
// contract, and the fallback for a repository that does not carry the capability
// (every case above drives that fallback leg, since auditFake does not implement
// AnchoredChainAppender).

// anchoredAuditFake wraps auditFake with the AnchoredChainAppender capability,
// returning a caller-supplied error from the anchored append. It is the ONLY
// fake in this file that implements the capability, so every other case here
// continues to exercise the non-capable fallback path.
type anchoredAuditFake struct {
	*auditFake
	err error
}

func (a *anchoredAuditFake) AppendChainedAnchored(ctx context.Context, p audit.ChainAppendParams,
	_ audit.AnchorSpec) (*audit.Entry, error) {
	if a.err != nil {
		return nil, a.err
	}
	return a.AppendChained(ctx, p)
}

// newAnchoredArbitrationServer wires a Server whose audit repo carries the
// capability and returns anchoredErr from the anchored append.
func newAnchoredArbitrationServer(t *testing.T, anchoredErr error) (*Server, *autoDriveRepo, *auditFake) {
	t.Helper()
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	s := New(Config{
		Addr: "127.0.0.1:0", RunRepo: repo,
		AuditRepo:    &anchoredAuditFake{auditFake: au, err: anchoredErr},
		Orchestrator: &orchestrator.Orchestrator{Runs: repo},
		GateMerger:   &fakeMerger{},
	})
	return s, repo, au
}

// TestAcceptanceArbitration_CapabilityPath_AnchorMoved: the primitive's
// *audit.AnchorMovedError maps to the SAME 409 acceptance_outcome_superseded
// wire contract the pre-#2536 non-atomic re-read produced, naming both
// sequences, with zero rows written.
func TestAcceptanceArbitration_CapabilityPath_AnchorMoved(t *testing.T) {
	moved := &audit.AnchorMovedError{
		Expected: arbitrationOutcomeSeq, Current: arbitrationOutcomeSeq + 40, Recorded: true,
	}
	s, repo, au := newAnchoredArbitrationServer(t, moved)
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("acceptance_outcome_superseded")) {
		t.Errorf("body missing acceptance_outcome_superseded: %s", w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"current_outcome_sequence":100`)) {
		t.Errorf("body must name the CURRENT sequence from the primitive: %s", w.Body.String())
	}
	if rows := arbitrationRows(au); len(rows) != 0 {
		t.Errorf("arbitration rows = %d, want 0", len(rows))
	}
}

// TestAcceptanceArbitration_CapabilityPath_Duplicate: the primitive's
// *audit.AnchoredDuplicateError maps to 200 already_recorded:true carrying the
// SURVIVING entry's sequence — the branch a concurrent POST that passed the
// endpoint fast path lands in.
func TestAcceptanceArbitration_CapabilityPath_Duplicate(t *testing.T) {
	const survivingSeq = int64(77)
	dup := &audit.AnchoredDuplicateError{Existing: &audit.Entry{
		Sequence: survivingSeq, Category: CategoryAcceptanceTriageArbitrated,
	}}
	s, repo, au := newAnchoredArbitrationServer(t, dup)
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var got acceptanceArbitrationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	if !got.AlreadyRecorded {
		t.Error("already_recorded = false, want true on a duplicate")
	}
	if got.ArbitrationSequence != survivingSeq {
		t.Errorf("arbitration_sequence = %d, want the surviving entry's %d", got.ArbitrationSequence, survivingSeq)
	}
	if rows := arbitrationRows(au); len(rows) != 0 {
		t.Errorf("arbitration rows = %d, want 0 (the duplicate branch writes nothing)", len(rows))
	}
}

// TestAcceptanceArbitration_CapabilityPath_OtherErrorIs500: any error that is
// neither typed error stays a hard 500 — a 23505 on an unrelated constraint (or
// the vanished-row integrity anomaly) must NOT be mistaken for the benign
// duplicate.
func TestAcceptanceArbitration_CapabilityPath_OtherErrorIs500(t *testing.T) {
	s, repo, au := newAnchoredArbitrationServer(t, errors.New("audit: duplicate on idx but no committed entry for outcome_sequence=60"))
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("internal_error")) {
		t.Errorf("body missing internal_error: %s", w.Body.String())
	}
	if rows := arbitrationRows(au); len(rows) != 0 {
		t.Errorf("arbitration rows = %d, want 0", len(rows))
	}
}

// TestAcceptanceArbitration_CapabilityPath_HappyPath: with the capability
// present and no error, the append still lands and the 200 reports it — so the
// capability path is not merely an error-mapping shim.
func TestAcceptanceArbitration_CapabilityPath_HappyPath(t *testing.T) {
	s, repo, au := newAnchoredArbitrationServer(t, nil)
	runID := uuid.New()
	seedPagedTriageRun(t, repo, au, runID, acceptanceClass5, acceptanceDispositionUnvalidatable, 0, 3)

	w := postArbitration(t, s, runID, acceptanceArbitrationRequest{Reason: class5Reason}, withMergeOperator)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	rows := arbitrationRows(au)
	if len(rows) != 1 {
		t.Fatalf("arbitration rows = %d, want 1", len(rows))
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`"already_recorded":true`)) {
		t.Errorf("already_recorded = true on a fresh append: %s", w.Body.String())
	}
}

// TestAcceptanceArbitrationRepoCarriesAnchoredCapability pins the fallback's
// PRECONDITION rather than asserting it in prose: the PRODUCTION audit
// repository must carry audit.AnchoredChainAppender, so the handler's
// non-capable fallback leg is reachable only by in-memory fakes. audit's own
// compile-time `var _ AnchoredChainAppender = (*postgresRepo)(nil)` is the other
// half; this asserts it across the package boundary the handler actually
// type-asserts on.
func TestAcceptanceArbitrationRepoCarriesAnchoredCapability(t *testing.T) {
	repo := audit.NewPostgresRepository(nil)
	if _, ok := repo.(audit.AnchoredChainAppender); !ok {
		t.Fatal("the production audit.Repository must implement audit.AnchoredChainAppender; without it the arbitration endpoint silently degrades to the non-atomic re-read-then-append")
	}
}
