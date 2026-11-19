package server

// Waive-proposal decisions (E80.28 / #5229, ADR-143 A1): confirm, reject, the
// agent_alternative pointer and the route registration, driven through the
// REAL middleware (s.Handler(): bearer auth, session cookie, CSRF and the
// requireConcernAccount resolution) on the session-only fixture, whose
// AuditRepo is the guarded test wrapper.
//
// Every test seeds state BY CONSTRUCTION (a raised server_check row, a
// pull_request_opened head row, fixup_pushed rows and concern_waive_proposed
// rows on the wrapper's chain) and reads committed state back after the call:
// the row's state, judgement and state_reason, and the audit rows per
// category. A response code alone never decides a test.
//
// COUNTERFACTUALS (hand-run; each mutation restored byte-identically): see the
// PR notes and the counterfactual sidecar for the observed RED output.
//
//   - H1 (confirm human-channel check): the confirm handler's
//     refuseNonHumanServerCheckClear call prefixed `false &&`. Fixture: a live
//     proposal at the current head A, an open row, a write:stages bearer.
//     _AgentTokenRefused RED (200, row waived, one concern_waived row), and the
//     confirm arms of TestServerCheckClearSession_RefusesBearerKinds RED.
//   - H2 (V1 confirm in-lock head check): (A) the confirm guard's
//     `cur != live.HeadSHA` made false; (B) applyConcernWaiveLinked called with
//     a nil guard (plain AppendChained). The hook records fixup_pushed{B} on the
//     first concern_waived append attempt, after the pre-check.
//     _PushBetweenPrecheckAndAppendRefused RED under both (200, row waived).
//   - H5 (fail closed without the capability): applyConcernWaiveLinked's
//     `return nil, errGuardedAppendUnavailable` replaced by a nil-guard
//     fallback. Bare fake, no wrapper: _NoGuardedAppenderFailsClosed/fresh-head
//     RED (200, one concern_waived row).
//   - H6 (V2 failed-intent reopen, waive_proposal.go): the fold's
//     `st.live[p.ConcernID] = p` replaced by `_ = p`.
//     _RetryAfterFailedDisposition RED ("a failed disposition closed the
//     proposal (V2)").
//   - H7 (acknowledged proposal keeps its place): the fold's reopen condition
//     `st.live[p.ConcernID] == nil` replaced by `true`. The hook proposes Q
//     between P's committed intent and its corrective row:
//     _ProposeDuringFailedDispositionKeepsNewer RED (live = P seq 2, want Q).
//   - C1 (confirm reason credential scan): decodeDecisionBody's
//     `*reason != ""` restored to `reasonRequired`.
//     _CredentialReasonRefused RED (200, row waived, state_reason carries the
//     credential bytes, the response echoes them).
//   - Y1 (state_reason never agent text): stateReason seeded from live.Reason.
//     _StateReasonNeverAgentText RED (state_reason, the concern_waived reason
//     and the settled-concerns review ledger carry the agent's marker).
//   - Y4 (delegation shadow): the recordDelegationShadow call deleted.
//     _SessionHumanWaives RED (0 delegation_shadow_evaluated rows).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Agent-authored markers: neither may reach state_reason or a review prompt.
const (
	agentReasonMarker   = "AGENT-REASON-7f3c9e: the matched string is a placeholder"
	agentEvidenceMarker = "AGENT-EVIDENCE-91ab4d: config/settings.go:2"
)

// seedAgentProposal appends a concern_waive_proposed row for row at head with
// the operator agent as actor, returning it with its chain sequence.
func seedAgentProposal(t *testing.T, w *guardedAuditWrapper, row *concern.Concern, head string) *audit.Entry {
	t.Helper()
	return seedAgentProposalJudgement(t, w, row, head, "no_defect")
}

func seedAgentProposalJudgement(t *testing.T, w *guardedAuditWrapper, row *concern.Concern, head, judgement string) *audit.Entry {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"concern_id": row.ID.String(), "stage_id": row.StageID.String(), "provenance": row.Provenance,
		"check_key": row.CheckKey, "category": row.Category, "severity": row.Severity,
		"reason": agentReasonMarker, "evidence": agentEvidenceMarker, "judgement": judgement, "head_sha": head,
	})
	kind := audit.ActorAgent
	subject := operatorAgentSubject
	w.gmu.Lock()
	defer w.gmu.Unlock()
	e, err := w.appendLocked(context.Background(), audit.ChainAppendParams{
		RunID: row.RunID, StageID: &row.StageID, Timestamp: time.Now().UTC(), Category: CategoryConcernWaiveProposed,
		ActorKind: &kind, ActorSubject: &subject, Payload: payload,
	})
	if err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return e
}

func confirmPath(row *concern.Concern) string {
	return "/v0/concerns/" + row.ID.String() + "/waive-proposal/confirm"
}

func rejectPath(row *concern.Concern) string {
	return "/v0/concerns/" + row.ID.String() + "/waive-proposal/reject"
}

func wpConfirmBody(seq int64) string { return jsonBody(map[string]any{"proposal_sequence": seq}) }

func rejectBody(seq int64) string {
	return jsonBody(map[string]any{"proposal_sequence": seq, "reason": "the evidence does not show the value is inert"})
}

func (fx *sessionOnlyFixture) live(t *testing.T, row *concern.Concern) *waiveProposal {
	t.Helper()
	st, err := loadWaiveProposalState(context.Background(), fx.gau, row.RunID)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	return st.Live(row.ID)
}

func (fx *sessionOnlyFixture) operatorAgentBearer(t *testing.T) string {
	t.Helper()
	return fx.bearerWithAuthMethod(t, operatorAgentSubject, "static")
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v: %s", err, w.Body.String())
	}
	return m
}

// ---- Confirm ----

func TestWaiveProposalConfirm_SessionHumanWaives(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	seq := confirmProposalSeq(row)

	w := fx.do(t, confirmPath(row), wpConfirmBody(seq), "", true, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w)
	human, _ := resp["decided_by"].(string)
	if human == "" || isAgentSubject(human) || resp["credential_kind"] != identity.CredentialSession ||
		resp["proposal_sequence"] != float64(seq) || resp["state"] != string(concern.StateWaived) {
		t.Errorf("response = %v, want decided_by the human, credential_kind session, proposal_sequence %d, state waived", resp, seq)
	}
	got := readConcern(t, fx.cr, row.ID)
	if got.State != concern.StateWaived || string(got.Judgement) != "no_defect" || got.JudgementActorKind != string(audit.ActorUser) {
		t.Errorf("row = state %s judgement %q actor %q; want waived, no_defect, user", got.State, got.Judgement, got.JudgementActorKind)
	}
	idx := auditEntriesByCategory(fx.au, CategoryConcernWaived)
	if len(idx) != 1 {
		t.Fatalf("concern_waived rows = %d, want 1", len(idx))
	}
	ap := fx.au.appended[idx[0]]
	if ap.ActorSubject == nil || *ap.ActorSubject != human || ap.ActorKind == nil || *ap.ActorKind != audit.ActorUser {
		t.Errorf("intent actor = %v/%v, want %s/user", ap.ActorSubject, ap.ActorKind, human)
	}
	p := fx.gau.payloads(t, CategoryConcernWaived)[0]
	for k, want := range map[string]any{
		"proposal_sequence": float64(seq), "proposed_by": operatorAgentSubject,
		"proposed_by_actor_kind": "agent", "proposal_head_sha": proposalHeadA,
	} {
		if p[k] != want {
			t.Errorf("intent payload %s = %v, want %v", k, p[k], want)
		}
	}
	if live := fx.live(t, row); live != nil {
		t.Errorf("proposal still live after confirm: %+v", live)
	}
	// Y4: the E82.1 delegation shadow is recorded, as for a direct waive.
	shadows := fx.gau.payloads(t, CategoryDelegationShadowEvaluated)
	if len(shadows) != 1 {
		t.Fatalf("delegation_shadow_evaluated rows = %d, want 1", len(shadows))
	}
	if shadows[0]["decision_category"] != CategoryConcernWaived || shadows[0]["human_decision"] != "waive" {
		t.Errorf("shadow payload = %v, want human_decision waive, decision_category concern_waived", shadows[0])
	}
}

// TestWaiveProposalConfirm_StateReasonNeverAgentText (Y1): the agent's reason
// and evidence never reach the concern's state_reason, the concern_waived
// reason, or the settled-concerns ledger rendered into later re-review
// prompts ("operator waive reason"). Without a human reason the server
// template is recorded; with one, the human's text is.
func TestWaiveProposalConfirm_StateReasonNeverAgentText(t *testing.T) {
	for _, tc := range []struct{ name, humanReason string }{{"template", ""}, {"human-reason", "I checked the value; it is the README example"}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			row := seedProposedServerCheck(t, fx, uuid.New())
			seq := confirmProposalSeq(row)
			w := fx.do(t, confirmPath(row), jsonBody(map[string]any{"proposal_sequence": seq, "reason": tc.humanReason}), "", true, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			human, _ := decodeJSON(t, w)["decided_by"].(string)
			want := tc.humanReason
			if want == "" {
				want = confirmWaiveProposalStateReason(seq, human)
			}
			got := readConcern(t, fx.cr, row.ID)
			intentReason, _ := fx.gau.payloads(t, CategoryConcernWaived)[0]["reason"].(string)
			var rendered []string
			for _, pc := range fx.s.settledConcernsForReview(context.Background(), row.RunID, row.StageID) {
				rendered = append(rendered, pc.StateReason, pc.Note)
			}
			if len(rendered) == 0 {
				t.Fatal("settledConcernsForReview rendered nothing; the waived row must reach the review ledger")
			}
			for name, text := range map[string]string{"state_reason": got.StateReason, "concern_waived reason": intentReason, "review ledger": strings.Join(rendered, "\n"), "response": w.Body.String()} {
				for _, marker := range []string{"AGENT-REASON-7f3c9e", "AGENT-EVIDENCE-91ab4d"} {
					if strings.Contains(text, marker) {
						t.Errorf("%s carries agent-authored text %q: %s", name, marker, text)
					}
				}
			}
			if got.StateReason != want || intentReason != want {
				t.Errorf("state_reason = %q, intent reason = %q; want %q", got.StateReason, intentReason, want)
			}
		})
	}
}

func TestWaiveProposalConfirm_AgentTokenRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())

	w := fx.do(t, confirmPath(row), wpConfirmBody(confirmProposalSeq(row)), fx.operatorAgentBearer(t), false, nil)

	assertRequiresHuman(t, w, row, clearVerbConfirmWaiveProposal, refusedActorAgentToken)
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
	if fx.live(t, row) == nil {
		t.Error("the proposal is no longer live after a refused agent confirm")
	}
}

func TestWaiveProposalConfirm_DelegatedSessionRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	w := fx.do(t, confirmPath(row), jsonBody(map[string]any{"proposal_sequence": confirmProposalSeq(row), "delegated": true}), "", true, nil)
	assertRequiresHuman(t, w, row, clearVerbConfirmWaiveProposal, refusedActorDelegated)
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
}

func TestWaiveProposalConfirm_StaleHeadRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	seq := confirmProposalSeq(row)
	fx.gau.seed(t, row.RunID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})

	env := assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(seq), "", true, nil), http.StatusConflict, "waive_proposal_stale")

	if env.Details["proposal_head_sha"] != proposalHeadA || env.Details["current_head_sha"] != proposalHeadB || env.Details["dropped_recorded"] != true {
		t.Errorf("details = %v, want both heads and dropped_recorded true", env.Details)
	}
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
	drops := auditEntriesByCategory(fx.au, CategoryConcernWaiveProposalDropped)
	if len(drops) != 1 {
		t.Fatalf("dropped rows = %d, want 1", len(drops))
	}
	if k := fx.au.appended[drops[0]].ActorKind; k == nil || *k != audit.ActorSystem {
		t.Errorf("drop actor = %v, want system", k)
	}
	assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(seq), "", true, nil), http.StatusNotFound, "waive_proposal_not_found")
}

// TestWaiveProposalConfirm_PushBetweenPrecheckAndAppendRefused (V1): the
// pre-check sees head A; the hook records fixup_pushed{B} at the first
// concern_waived append attempt, before the in-lock re-read. Nothing is
// appended and the disposition never runs.
func TestWaiveProposalConfirm_PushBetweenPrecheckAndAppendRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	fx.gau.beforeAppend(CategoryConcernWaived, func() {
		fx.gau.seed(t, row.RunID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	})

	env := assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(confirmProposalSeq(row)), "", true, nil), http.StatusConflict, "waive_proposal_stale")

	if env.Details["dropped_recorded"] != false || env.Details["current_head_sha"] != proposalHeadB {
		t.Errorf("details = %v, want dropped_recorded false and current head B", env.Details)
	}
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
	for _, cat := range []string{CategoryConcernWaiveFailed, CategoryConcernWaiveProposalDropped} {
		if n := fx.gau.count(cat); n != 0 {
			t.Errorf("%s rows = %d, want 0", cat, n)
		}
	}
}

func TestWaiveProposalConfirm_ProposalClosedUnderLockRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	seq := confirmProposalSeq(row)
	fx.gau.beforeAppend(CategoryConcernWaived, func() {
		fx.gau.seed(t, row.RunID, CategoryConcernWaiveProposalRejected, map[string]any{"concern_id": row.ID.String(), "proposal_sequence": seq})
	})
	assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(seq), "", true, nil), http.StatusConflict, "waive_proposal_not_live")
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
}

// TestWaiveProposalConfirm_RetryAfterFailedDisposition (V2): a failed
// disposition leaves the proposal live and the human's retry succeeds.
func TestWaiveProposalConfirm_RetryAfterFailedDisposition(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	seq := confirmProposalSeq(row)
	fx.cr.applyHumanDispositionErr = errNoCapStub("transient store failure")

	w := fx.do(t, confirmPath(row), wpConfirmBody(seq), "", true, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("first confirm status = %d, want 500: %s", w.Code, w.Body.String())
	}
	intents := auditEntriesByCategory(fx.au, CategoryConcernWaived)
	failed := fx.gau.payloads(t, CategoryConcernWaiveFailed)
	if len(intents) != 1 || len(failed) != 1 {
		t.Fatalf("intents = %d, failed = %d; want 1 and 1", len(intents), len(failed))
	}
	if failed[0]["intent_sequence"] != float64(intents[0]+1) {
		t.Errorf("corrective intent_sequence = %v, want %d", failed[0]["intent_sequence"], intents[0]+1)
	}
	if fx.live(t, row) == nil {
		t.Fatal("a failed disposition closed the proposal (V2)")
	}
	assertState(t, fx.cr, row.ID, concern.StateRaised)

	fx.cr.applyHumanDispositionErr = nil
	if w := fx.do(t, confirmPath(row), wpConfirmBody(seq), "", true, nil); w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200: %s", w.Code, w.Body.String())
	}
	fx.assertCommitted(t, row, concern.StateWaived, CategoryConcernWaived, 2)
}

// TestWaiveProposalConfirm_ProposeDuringFailedDispositionKeepsNewer: a confirm
// of P commits its linked intent, its disposition fails, and BEFORE its
// corrective concern_waive_failed row lands the operator agent proposes Q
// (the hook on that row's append: the intent is committed, so the fold reads
// P closed and the propose answers 201). The acknowledged Q must stay live and
// decidable; P stays closed (superseded), and a retry of P names Q.
func TestWaiveProposalConfirm_ProposeDuringFailedDispositionKeepsNewer(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	p := confirmProposalSeq(row)
	fx.cr.applyHumanDispositionErr = errNoCapStub("transient store failure")
	var q int64
	fx.gau.beforeAppend(CategoryConcernWaiveFailed, func() {
		w := fx.do(t, "/v0/concerns/"+row.ID.String()+"/waive-proposal", jsonBody(validProposeBody()), fx.operatorAgentBearer(t), false, nil)
		if w.Code != http.StatusCreated {
			t.Errorf("propose during the failed confirm = %d, want 201: %s", w.Code, w.Body.String())
			return
		}
		q = int64(decodeJSON(t, w)["proposal"].(map[string]any)["proposal_sequence"].(float64))
	})

	if w := fx.do(t, confirmPath(row), wpConfirmBody(p), "", true, nil); w.Code != http.StatusInternalServerError {
		t.Fatalf("confirm of P status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if q == 0 {
		t.Fatal("the hook did not record Q")
	}
	if n := fx.gau.count(CategoryConcernWaiveFailed); n != 1 {
		t.Fatalf("concern_waive_failed rows = %d, want 1 (the corrective row landed after Q)", n)
	}
	st, err := loadWaiveProposalState(context.Background(), fx.gau, row.RunID)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if live := st.Live(row.ID); live == nil || live.Sequence != q {
		t.Fatalf("live proposal = %+v, want the acknowledged Q (seq %d), not P (seq %d)", live, q, p)
	}
	if n := st.inert[row.ID]; n != 0 {
		t.Errorf("inert proposals = %d, want 0 (an acknowledged proposal never turns inert)", n)
	}
	assertState(t, fx.cr, row.ID, concern.StateRaised)

	fx.cr.applyHumanDispositionErr = nil
	env := assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(p), "", true, nil), http.StatusConflict, "waive_proposal_not_live")
	if env.Details["live_proposal_sequence"] != float64(q) {
		t.Errorf("retry of P: details.live_proposal_sequence = %v, want %d", env.Details["live_proposal_sequence"], q)
	}
	if w := fx.do(t, confirmPath(row), wpConfirmBody(q), "", true, nil); w.Code != http.StatusOK {
		t.Fatalf("confirm of Q status = %d, want 200: %s", w.Code, w.Body.String())
	}
	fx.assertCommitted(t, row, concern.StateWaived, CategoryConcernWaived, 2)
}

// TestWaiveProposalConfirm_CredentialReasonRefused: the confirm's optional
// human reason becomes state_reason, which re-review prompts render, so a
// credential-shaped reason is refused before any append and the bytes are not
// echoed.
func TestWaiveProposalConfirm_CredentialReasonRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	secret := generatedPAT(t)
	body := jsonBody(map[string]any{"proposal_sequence": confirmProposalSeq(row), "reason": "the value " + secret + " is inert"})

	w := fx.do(t, confirmPath(row), body, "", true, nil)

	env := assertErrCode(t, w, http.StatusBadRequest, "waive_proposal_contains_credential")
	if env.Details["field"] != "reason" {
		t.Errorf("details.field = %v, want reason", env.Details["field"])
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Error("the response echoes the credential-shaped bytes")
	}
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
	if got := readConcern(t, fx.cr, row.ID).StateReason; strings.Contains(got, secret) {
		t.Errorf("state_reason carries the credential-shaped bytes: %q", got)
	}
	if fx.live(t, row) == nil {
		t.Error("the proposal is no longer live after a refused confirm")
	}
}

// TestWaiveProposalConfirm_CorrectiveWriteFailureNotSuccess (Y2): when the
// corrective concern_waive_failed write fails too, the handler still returns
// an error, never success; the row stays open. The fold then closes the
// proposal (the stated residual).
func TestWaiveProposalConfirm_CorrectiveWriteFailureNotSuccess(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	fx.cr.applyHumanDispositionErr = errNoCapStub("transient store failure")
	fx.au.appendErrCategory = CategoryConcernWaiveFailed

	w := fx.do(t, confirmPath(row), wpConfirmBody(confirmProposalSeq(row)), "", true, nil)

	if w.Code == http.StatusOK || w.Code < 400 {
		t.Fatalf("status = %d, want an error (never success): %s", w.Code, w.Body.String())
	}
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 1)
	if n := fx.gau.count(CategoryConcernWaiveFailed); n != 0 {
		t.Errorf("concern_waive_failed rows = %d, want 0 (the write was injected to fail)", n)
	}
	if fx.live(t, row) != nil {
		t.Error("residual changed: the fold kept the proposal live without a recorded failure; update A1 Consequences and review.md")
	}
}

func TestWaiveProposalConfirm_NotLiveSequenceRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	p1 := confirmProposalSeq(row)
	fx.gau.seed(t, row.RunID, CategoryConcernWaiveProposalRejected, map[string]any{"concern_id": row.ID.String(), "proposal_sequence": p1})
	p2 := seedAgentProposal(t, fx.gau, row, proposalHeadA)

	env := assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(p1), "", true, nil), http.StatusConflict, "waive_proposal_not_live")

	if env.Details["live_proposal_sequence"] != float64(p2.Sequence) {
		t.Errorf("details.live_proposal_sequence = %v, want %d", env.Details["live_proposal_sequence"], p2.Sequence)
	}
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
}

func TestWaiveProposalConfirm_NoProposalRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedRaisedServerCheck(t, fx, uuid.New())
	fx.gau.seed(t, row.RunID, "pull_request_opened", map[string]any{"head_sha": proposalHeadA})
	assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(1), "", true, nil), http.StatusNotFound, "waive_proposal_not_found")
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
}

func TestWaiveProposalConfirm_ConcernNotOpenRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	if _, err := fx.cr.ApplyHumanDisposition(context.Background(), row.ID, concern.StateWaived, "human waived directly", concern.HumanDisposition{
		Outcome: concern.OutcomeWaived, Judgement: concern.JudgementNoDefect, ActorKind: string(audit.ActorUser),
	}); err != nil {
		t.Fatalf("seed waived: %v", err)
	}
	assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(confirmProposalSeq(row)), "", true, nil), http.StatusConflict, "concern_not_open")
	fx.assertCommitted(t, row, concern.StateWaived, CategoryConcernWaived, 0)
}

func TestWaiveProposalConfirm_ReviewerConcernRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	runID := uuid.New()
	seedDeferRun(fx.runs, runID)
	row := seedConcernRow(t, fx.cr, runID, uuid.New(), concern.StageKindImplement, 1, "naming nit")
	assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(1), "", true, nil), http.StatusUnprocessableEntity, "concern_not_server_check")
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
}

func TestWaiveProposalConfirm_BodyRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"sequence-missing": jsonBody(map[string]any{}),
		"sequence-zero":    wpConfirmBody(0),
		"reason-too-long":  jsonBody(map[string]any{"proposal_sequence": 1, "reason": strings.Repeat("r", maxOperatorConcernBytes+1)}),
		"not-json":         "{",
	} {
		t.Run(name, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			row := seedProposedServerCheck(t, fx, uuid.New())
			assertErrCode(t, fx.do(t, confirmPath(row), body, "", true, nil), http.StatusBadRequest, "validation_failed")
			fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
		})
	}
}

func TestWaiveProposalConfirm_HeadReadErrorFailsClosed(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	fx.au.listByCategoryErrCategory = "pull_request_opened"
	assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(confirmProposalSeq(row)), "", true, nil), http.StatusInternalServerError, "internal_error")
	fx.au.listByCategoryErrCategory = ""
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
}

func TestWaiveProposalConfirm_InvalidRecordedJudgementFailsClosed(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedRaisedServerCheck(t, fx, uuid.New())
	fx.gau.seed(t, row.RunID, "pull_request_opened", map[string]any{"head_sha": proposalHeadA})
	p := seedAgentProposalJudgement(t, fx.gau, row, proposalHeadA, "not_a_judgement")
	assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(p.Sequence), "", true, nil), http.StatusInternalServerError, "internal_error")
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
}

// TestWaiveProposalConfirm_NoGuardedAppenderFailsClosed: a repository without
// audit.GuardedChainAppender answers 503 on both the main path and the
// stale-drop path, and appends nothing (no non-atomic fallback, V1).
func TestWaiveProposalConfirm_NoGuardedAppenderFailsClosed(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "fresh-head"
		if stale {
			name = "stale-head"
		}
		t.Run(name, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			row := seedProposedServerCheck(t, fx, uuid.New())
			if stale {
				fx.gau.seed(t, row.RunID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
			}
			fx.s.cfg.AuditRepo = fx.au // the bare fake: no GuardedChainAppender
			assertErrCode(t, fx.do(t, confirmPath(row), wpConfirmBody(confirmProposalSeq(row)), "", true, nil),
				http.StatusServiceUnavailable, "waive_proposal_atomic_append_unavailable")
			fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
			if n := fx.gau.count(CategoryConcernWaiveProposalDropped); n != 0 {
				t.Errorf("dropped rows = %d, want 0", n)
			}
		})
	}
}

// ---- Reject ----

func TestWaiveProposalReject_SessionHumanRejects(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	seq := confirmProposalSeq(row)

	w := fx.do(t, rejectPath(row), rejectBody(seq), "", true, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	resp := decodeJSON(t, w)
	if resp["state"] != "rejected" || resp["proposal_sequence"] != float64(seq) || resp["credential_kind"] != identity.CredentialSession {
		t.Errorf("response = %v", resp)
	}
	idx := auditEntriesByCategory(fx.au, CategoryConcernWaiveProposalRejected)
	if len(idx) != 1 {
		t.Fatalf("rejected rows = %d, want 1", len(idx))
	}
	ap := fx.au.appended[idx[0]]
	if ap.ActorKind == nil || *ap.ActorKind != audit.ActorUser || ap.ActorSubject == nil || *ap.ActorSubject != resp["decided_by"] {
		t.Errorf("reject actor = %v/%v, want the human user", ap.ActorSubject, ap.ActorKind)
	}
	if p := fx.gau.payloads(t, CategoryConcernWaiveProposalRejected)[0]; p["proposal_sequence"] != float64(seq) || p["reason"] == "" {
		t.Errorf("reject payload = %v", p)
	}
	fx.assertCommitted(t, row, concern.StateRaised, CategoryConcernWaived, 0)
	if fx.live(t, row) != nil {
		t.Error("the proposal is still live after a reject")
	}
	if w := postPropose(t, fx.s, row.ID.String(), validProposeBody(), withOperatorAgentAuth); w.Code != http.StatusCreated {
		t.Errorf("a new proposal after the reject = %d, want 201: %s", w.Code, w.Body.String())
	}
}

func TestWaiveProposalReject_AgentTokenRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	w := fx.do(t, rejectPath(row), rejectBody(confirmProposalSeq(row)), fx.operatorAgentBearer(t), false, nil)
	assertRequiresHuman(t, w, row, clearVerbRejectWaiveProposal, refusedActorAgentToken)
	if n := fx.gau.count(CategoryConcernWaiveProposalRejected); n != 0 {
		t.Errorf("rejected rows = %d, want 0", n)
	}
	if fx.live(t, row) == nil {
		t.Error("the proposal is no longer live after a refused agent reject")
	}
}

func TestWaiveProposalReject_StaticBearerRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	w := fx.do(t, rejectPath(row), rejectBody(confirmProposalSeq(row)), fx.bearerFor(t, identity.CredentialStatic, row.RunID), false, nil)
	assertRequiresSession(t, w, row, clearVerbRejectWaiveProposal, identity.CredentialStatic)
	if n := fx.gau.count(CategoryConcernWaiveProposalRejected); n != 0 {
		t.Errorf("rejected rows = %d, want 0", n)
	}
}

func TestWaiveProposalReject_MissingCSRFRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	req := httptest.NewRequest(http.MethodPost, rejectPath(row), strings.NewReader(rejectBody(confirmProposalSeq(row))))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(fx.session)
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: fx.csrf})
	w := httptest.NewRecorder()
	fx.s.Handler().ServeHTTP(w, req)
	assertErrCode(t, w, http.StatusForbidden, "csrf_required")
	if n := fx.gau.count(CategoryConcernWaiveProposalRejected); n != 0 {
		t.Errorf("rejected rows = %d, want 0", n)
	}
}

func TestWaiveProposalReject_DelegatedSessionRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	body := jsonBody(map[string]any{"proposal_sequence": confirmProposalSeq(row), "reason": "no", "delegated": true})
	assertRequiresHuman(t, fx.do(t, rejectPath(row), body, "", true, nil), row, clearVerbRejectWaiveProposal, refusedActorDelegated)
	if n := fx.gau.count(CategoryConcernWaiveProposalRejected); n != 0 {
		t.Errorf("rejected rows = %d, want 0", n)
	}
}

func TestWaiveProposalReject_BodyRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		body func(seq int64) map[string]any
		code string
	}{
		"reason-required": {func(seq int64) map[string]any { return map[string]any{"proposal_sequence": seq, "reason": "  "} }, "validation_failed"},
		"reason-too-long": {func(seq int64) map[string]any {
			return map[string]any{"proposal_sequence": seq, "reason": strings.Repeat("r", maxOperatorConcernBytes+1)}
		}, "validation_failed"},
		"sequence-missing": {func(int64) map[string]any { return map[string]any{"reason": "no"} }, "validation_failed"},
		"credential":       {nil, "waive_proposal_contains_credential"},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newSessionOnlyFixture(t)
			row := seedProposedServerCheck(t, fx, uuid.New())
			body := tc.body
			secret := generatedPAT(t)
			if body == nil {
				body = func(seq int64) map[string]any {
					return map[string]any{"proposal_sequence": seq, "reason": "the value " + secret + " is inert"}
				}
			}
			w := fx.do(t, rejectPath(row), jsonBody(body(confirmProposalSeq(row))), "", true, nil)
			assertErrCode(t, w, http.StatusBadRequest, tc.code)
			if strings.Contains(w.Body.String(), secret) {
				t.Error("the response echoes the credential-shaped bytes")
			}
			if n := fx.gau.count(CategoryConcernWaiveProposalRejected); n != 0 {
				t.Errorf("rejected rows = %d, want 0", n)
			}
			if fx.live(t, row) == nil {
				t.Error("the proposal is no longer live after a refused reject")
			}
		})
	}
}

// TestWaiveProposalReject_AfterConfirmUnderLockRefused: a linked confirm lands
// after the reject's pre-check; the in-lock guard refuses and nothing is
// appended.
func TestWaiveProposalReject_AfterConfirmUnderLockRefused(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	seq := confirmProposalSeq(row)
	fx.gau.beforeAppend(CategoryConcernWaiveProposalRejected, func() {
		fx.gau.seed(t, row.RunID, CategoryConcernWaived, map[string]any{"concern_id": row.ID.String(), "proposal_sequence": seq})
	})
	assertErrCode(t, fx.do(t, rejectPath(row), rejectBody(seq), "", true, nil), http.StatusConflict, "waive_proposal_not_live")
	if n := fx.gau.count(CategoryConcernWaiveProposalRejected); n != 0 {
		t.Errorf("rejected rows = %d, want 0", n)
	}
}

func TestWaiveProposalReject_NoGuardedAppenderFailsClosed(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	fx.s.cfg.AuditRepo = fx.au
	assertErrCode(t, fx.do(t, rejectPath(row), rejectBody(confirmProposalSeq(row)), "", true, nil),
		http.StatusServiceUnavailable, "waive_proposal_atomic_append_unavailable")
	if n := fx.gau.count(CategoryConcernWaiveProposalRejected); n != 0 {
		t.Errorf("rejected rows = %d, want 0", n)
	}
}

// ---- Routes and pointer ----

// TestWaiveProposalRoutes_RegisteredBehindAccountAndCSRF: the three routes
// reach their handlers through s.Handler(), and a session from another
// account gets 403 account_forbidden from requireConcernAccount with nothing
// appended.
func TestWaiveProposalRoutes_RegisteredBehindAccountAndCSRF(t *testing.T) {
	fx := newSessionOnlyFixture(t)
	row := seedProposedServerCheck(t, fx, uuid.New())
	seq := confirmProposalSeq(row)
	if w := fx.do(t, rejectPath(row), rejectBody(seq), "", true, nil); w.Code != http.StatusOK {
		t.Fatalf("reject route = %d, want 200: %s", w.Code, w.Body.String())
	}
	w := fx.do(t, "/v0/concerns/"+row.ID.String()+"/waive-proposal", jsonBody(validProposeBody()), "", true, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("propose route = %d, want 201: %s", w.Code, w.Body.String())
	}
	next := int64(decodeJSON(t, w)["proposal"].(map[string]any)["proposal_sequence"].(float64))
	if w := fx.do(t, confirmPath(row), wpConfirmBody(next), "", true, nil); w.Code != http.StatusOK {
		t.Fatalf("confirm route = %d, want 200: %s", w.Code, w.Body.String())
	}

	// Cross-account: the run belongs to account B; the session is bound to another.
	other := newSessionOnlyFixture(t)
	runID := uuid.New()
	other.runs.seedRun(&run.Run{ID: runID, State: run.StateRunning, AccountID: authzAcctB})
	xrow := seedServerCheckRow(t, other.cr, runID, uuid.New(), "high")
	other.gau.seed(t, runID, "pull_request_opened", map[string]any{"head_sha": proposalHeadA})
	xp := seedAgentProposal(t, other.gau, xrow, proposalHeadA)
	for _, path := range []string{confirmPath(xrow), rejectPath(xrow), "/v0/concerns/" + xrow.ID.String() + "/waive-proposal"} {
		body := rejectBody(xp.Sequence)
		if strings.HasSuffix(path, "/waive-proposal") {
			body = jsonBody(validProposeBody())
		}
		if w := other.do(t, path, body, "", true, nil); w.Code != http.StatusForbidden || !bodyHasCode(w, "account_forbidden") {
			t.Errorf("cross-account %s = %d, want 403 account_forbidden: %s", path, w.Code, w.Body.String())
		}
	}
	other.assertCommitted(t, xrow, concern.StateRaised, CategoryConcernWaived, 0)
	for _, cat := range []string{CategoryConcernWaiveProposalRejected, CategoryConcernWaiveProposed} {
		want := 0
		if cat == CategoryConcernWaiveProposed {
			want = 1 // the seeded one
		}
		if n := other.gau.count(cat); n != want {
			t.Errorf("cross-account %s rows = %d, want %d", cat, n, want)
		}
	}
}

// TestServerCheckRefusal_NamesProposeAlternative: the four direct verbs'
// concern_requires_human and concern_requires_session refusals carry
// details.agent_alternative = fishhawk_propose_waive and name it in the
// message; the confirm and reject refusals do not.
func TestServerCheckRefusal_NamesProposeAlternative(t *testing.T) {
	type refusal struct {
		name       string
		vc         clearVerbCase
		wantPoint  bool
		pathAndBod func(row *concern.Concern) (string, string)
	}
	var cases []refusal
	for _, vc := range clearVerbCases {
		vc := vc
		cases = append(cases, refusal{name: vc.verb, vc: vc, wantPoint: !isWaiveProposalDecision(vc.verb),
			pathAndBod: func(row *concern.Concern) (string, string) { return vc.request(row, false) }})
	}
	cases = append(cases, refusal{name: clearVerbRejectWaiveProposal,
		vc: clearVerbCase{verb: clearVerbRejectWaiveProposal, seed: seedProposedServerCheck},
		pathAndBod: func(row *concern.Concern) (string, string) {
			return rejectPath(row), rejectBody(confirmProposalSeq(row))
		}})
	for _, rc := range cases {
		for _, bearerKind := range []string{"operator_agent", identity.CredentialStatic} {
			t.Run(rc.name+"/"+bearerKind, func(t *testing.T) {
				fx := newSessionOnlyFixture(t)
				row := rc.vc.seed(t, fx, uuid.New())
				path, body := rc.pathAndBod(row)
				bearer := fx.bearerFor(t, identity.CredentialStatic, row.RunID)
				wantCode := errCodeConcernRequiresSession
				if bearerKind == "operator_agent" {
					bearer = fx.operatorAgentBearer(t)
					wantCode = errCodeConcernRequiresHuman
				}
				w := fx.do(t, path, body, bearer, false, nil)
				if rc.vc.verb == clearVerbResolve && bearerKind == "operator_agent" {
					// resolve's pre-body guard answers resolve_requires_human first.
					assertErrCode(t, w, http.StatusForbidden, errCodeResolveRequiresHuman)
					return
				}
				env := assertErrCode(t, w, http.StatusForbidden, wantCode)
				got, has := env.Details["agent_alternative"]
				if rc.wantPoint {
					if got != agentAlternativeProposeWaive || !strings.Contains(env.Message, "fishhawk_propose_waive") {
						t.Errorf("details.agent_alternative = %v, message %q; want %s named in both", got, env.Message, agentAlternativeProposeWaive)
					}
				} else if has || strings.Contains(env.Message, "fishhawk_propose_waive") {
					t.Errorf("a proposal decision's refusal points at %v (%q); an agent cannot decide its own proposal", got, env.Message)
				}
			})
		}
	}
}

// errNoCapStub is a plain error for the concern fake's injected failures.
type errNoCapStub string

func (e errNoCapStub) Error() string { return string(e) }

// readConcern reads row id back from the concern fake.
func readConcern(t *testing.T, cr *fakeConcernRepo, id uuid.UUID) *concern.Concern {
	t.Helper()
	rows, err := cr.GetByIDs(context.Background(), []uuid.UUID{id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("read back concern %s: %v", id, err)
	}
	return rows[0]
}

// TestWaiveProposalConfirm_PG_IndexesDecision (X1): the production wiring
// over real Postgres. The AuditRepo is the decision-index decorator over
// audit.NewPostgresRepository, so the confirm's concern_waived runs through
// the decorator's forwarded AppendChainedGuarded under the real run-row lock
// and must land exactly ONE concern_waive decision_index row naming its
// sequence.
func TestWaiveProposalConfirm_PG_IndexesDecision(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	pgAudit := audit.NewPostgresRepository(pool)
	auditRepo, err := decisionindex.NewIndexingRepository(pgAudit, decisionindex.NewStore(pool),
		decisionindex.NewPoolResolver(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("indexing repository: %v", err)
	}
	concernRepo := concern.NewPostgresRepository(pool)
	authRepo := auth.NewPostgresRepository(pool)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, ConcernRepo: concernRepo, AuthRepo: authRepo})

	accountID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO accounts (id, provider, account_key) VALUES ($1, $2, $3)`,
		accountID, "github", "acme-"+accountID.String()[:8]); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	_, sess, err := authRepo.SignIn(ctx, "github", auth.GitHubProfile{ID: 4242, Login: "alice", Name: "Alice"}, accountID)
	if err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	csrfTok, err := generateCSRFToken()
	if err != nil {
		t.Fatalf("generateCSRFToken: %v", err)
	}
	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "cafef00d", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.TransitionRun(ctx, r.ID, run.StateRunning); err != nil {
		t.Fatalf("run -> running: %v", err)
	}
	impl, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("create stage: %v", err)
	}
	rows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: r.ID, StageID: impl.ID, StageKind: concern.StageKindImplement,
		Provenance: concern.ProvenanceServerCheck, OriginReviewSequence: 9,
		Concerns: []concern.RaisedConcern{{
			Severity: "high", Category: "diff_secrets_detected",
			Note: "credential-shaped addition (github-pat-classic) at config/settings.go:2", CheckKey: serverCheckKey,
		}},
	})
	if err != nil {
		t.Fatalf("seed server_check concern: %v", err)
	}
	row := rows[0]
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: r.ID, StageID: &impl.ID, Timestamp: time.Now().UTC(), Category: "pull_request_opened",
		Payload: []byte(`{"head_sha":"` + proposalHeadA + `"}`),
	}); err != nil {
		t.Fatalf("seed head: %v", err)
	}

	pw := postPropose(t, s, row.ID.String(), validProposeBody(), withOperatorAgentAuth)
	if pw.Code != http.StatusCreated {
		t.Fatalf("propose = %d, want 201: %s", pw.Code, pw.Body.String())
	}
	seq := int64(decodeJSON(t, pw)["proposal"].(map[string]any)["proposal_sequence"].(float64))

	req := httptest.NewRequest(http.MethodPost, confirmPath(row), strings.NewReader(wpConfirmBody(seq)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.PlainText})
	req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: csrfTok})
	req.Header.Set(CSRFHeaderName, csrfTok)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200: %s", w.Code, w.Body.String())
	}

	got, err := concernRepo.GetByIDs(ctx, []uuid.UUID{row.ID})
	if err != nil || got[0].State != concern.StateWaived {
		t.Fatalf("committed concern = %+v (err %v), want waived", got, err)
	}
	waived, err := pgAudit.ListForRunByCategory(ctx, r.ID, CategoryConcernWaived)
	if err != nil {
		t.Fatalf("read chain: %v", err)
	}
	if len(waived) != 1 || !strings.Contains(string(waived[0].Payload), `"proposal_sequence":`) {
		t.Fatalf("concern_waived entries = %d (%v), want exactly one proposal-linked intent", len(waived), waived)
	}
	var n int
	var class string
	var src int64
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(decision_class),''), coalesce(max(source_sequence),0) FROM decision_index WHERE run_id = $1 AND decision_class = $2`,
		r.ID, string(decisionindex.ClassConcernWaive)).Scan(&n, &class, &src); err != nil {
		t.Fatalf("read decision_index: %v", err)
	}
	if n != 1 || src != waived[0].Sequence {
		t.Fatalf("decision_index concern_waive rows = %d (class %q, source_sequence %d), want exactly 1 naming entry %d", n, class, src, waived[0].Sequence)
	}
}
