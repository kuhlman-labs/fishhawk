package server

// Waive-proposal read surfaces (E80.28 / #5229, ADR-143 A1): the gate view's
// per-concern waive_proposal block, the run read's waive_proposals list, the
// single recorded-head derivation, the "pending leaves the concern open"
// property, and the contract-doc pins (OpenAPI, v0.md, the A1 text).
//
// Every test seeds state BY CONSTRUCTION (a fake server_check concern row, a
// pull_request_opened head row, fixup_pushed rows and concern_waive_proposed
// rows on the guardedAuditWrapper chain) and reads the surface back. The
// attention-queue arms (both server-check paths) live in attention_test.go.
//
// COUNTERFACTUALS (hand-run; each mutation restored byte-identically):
//
//   - gate-view block wiring (gateview.go buildGateView): the
//     `open.WaiveProposal = &v` assignment removed. Fixture: a live proposal at
//     the recorded head A on an open server_check row. Observed RED:
//     TestGateView_WaiveProposalPending "open concern carries no waive_proposal".
//   - gate-view read-failure gap (gateViewWaiveProposals): the gap append
//     removed. Fixture: concern_waive_proposed reads fail. Observed RED:
//     TestGateView_WaiveProposalReadFailureRecordsGap "history_gaps ... want
//     waive_proposals".
//   - run-read wiring (runs.go handleGetRun): the waiveProposalsForRun call
//     removed. Observed RED: TestGetRun_WaiveProposalsPending "waive_proposals
//     absent".
//   - head delegation (gateview.go gateViewCurrentHead): the delegate body
//     replaced by `return "", nil`. Observed RED:
//     TestGateViewCurrentHead_DelegatesToRecordedRunHead.
//   - attention wiring (attention.go): `proposal = &v` replaced by `_ = v` on the
//     direct path, and `WaiveProposal: c.WaiveProposal` removed on the gate-view
//     path; the pendingWaiveProposals read replaced by an empty map. Observed
//     RED: TestAttention_ServerCheckCarriesWaiveProposal/direct and
//     /parked_gate_view "want context.waive_proposal".

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/diffsecrets"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const readsProposer = "operator-agent/1.0"

// readsFixture is a server with a run repository (so the run read resolves),
// the guarded audit wrapper and a fake concern store, seeded with one raised
// high server_check row and a pull_request_opened row naming head A.
type readsFixture struct {
	s       *Server
	repo    *fakeRepo
	au      *guardedAuditWrapper
	cr      *fakeConcernRepo
	runID   uuid.UUID
	stageID uuid.UUID
	row     *concern.Concern
}

func newReadsFixture(t *testing.T) *readsFixture {
	t.Helper()
	repo := newFakeRepo()
	au := newGuardedAuditWrapper()
	cr := newFakeConcernRepo()
	f := &readsFixture{
		s:    New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: cr}),
		repo: repo, au: au, cr: cr, stageID: uuid.New(),
	}
	f.runID = seedGateRun(t, repo)
	f.row = seedServerCheckRow(t, cr, f.runID, f.stageID, "high")
	au.seed(t, f.runID, "pull_request_opened", map[string]any{"head_sha": proposalHeadA})
	return f
}

// propose appends a concern_waive_proposed row for c at head, attributed to an
// operator agent, exactly as handleProposeWaive records it.
func (f *readsFixture) propose(t *testing.T, c *concern.Concern, head string) *audit.Entry {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"concern_id": c.ID.String(), "stage_id": c.StageID.String(), "stage_kind": c.StageKind,
		"provenance": c.Provenance, "check_key": c.CheckKey, "category": c.Category, "severity": c.Severity,
		"reason": "documented placeholder, not a live token", "judgement": "tradeoff",
		"tradeoff_cost": "none", "evidence": "config/settings.go:2 assigns the README example", "head_sha": head,
	})
	if err != nil {
		t.Fatal(err)
	}
	kind, subject, sid := audit.ActorAgent, readsProposer, c.StageID
	e, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.runID, StageID: &sid, Timestamp: time.Now().UTC(), Category: CategoryConcernWaiveProposed,
		Payload: payload, ActorKind: &kind, ActorSubject: &subject,
	})
	if err != nil {
		t.Fatalf("seed proposal: %v", err)
	}
	return e
}

// gateView builds the gate view over the run's full ledger and returns it
// decoded from its JSON wire form, so the tags are pinned with the values.
func (f *readsFixture) gateView(t *testing.T) gateViewResponse {
	t.Helper()
	rows, err := f.cr.ListByRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(f.s.buildGateView(context.Background(), f.runID, "", rows))
	if err != nil {
		t.Fatal(err)
	}
	var resp gateViewResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func openConcern(t *testing.T, resp gateViewResponse, id uuid.UUID) gateViewConcern {
	t.Helper()
	for _, c := range resp.Open {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("concern %s not in the gate view's open set: %+v", id, resp.Open)
	return gateViewConcern{}
}

func TestGateView_WaiveProposalPending(t *testing.T) {
	f := newReadsFixture(t)
	p := f.propose(t, f.row, proposalHeadA)
	resp := f.gateView(t)
	got := openConcern(t, resp, f.row.ID).WaiveProposal
	if got == nil {
		t.Fatalf("open concern carries no waive_proposal; gaps %v", resp.HistoryGaps)
	}
	want := waiveProposalView{
		ProposalSequence: p.Sequence, ConcernID: f.row.ID.String(), StageID: f.stageID.String(),
		ProposedBy: readsProposer, ProposedByActorKind: string(audit.ActorAgent), Judgement: "tradeoff",
		TradeoffCost: "none", Reason: "documented placeholder, not a live token",
		Evidence: "config/settings.go:2 assigns the README example", HeadSHA: proposalHeadA,
		CheckKey: serverCheckKey, Category: f.row.Category, Severity: "high", AgentAuthored: true,
	}
	gotCmp := *got
	gotCmp.ProposedAt = time.Time{}
	if gotCmp != want {
		t.Errorf("waive_proposal =\n %+v\nwant\n %+v", gotCmp, want)
	}
	if got.ProposedAt.IsZero() {
		t.Error("proposed_at is zero")
	}
	if resp.HistoryIncomplete {
		t.Errorf("history_incomplete with gaps %v, want a complete view", resp.HistoryGaps)
	}
	// The row itself is untouched: a pending proposal clears nothing.
	assertState(t, f.cr, f.row.ID, concern.StateRaised)
}

func TestGateView_WaiveProposalStaleOmitted(t *testing.T) {
	f := newReadsFixture(t)
	f.propose(t, f.row, proposalHeadA)
	f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	if got := openConcern(t, f.gateView(t), f.row.ID).WaiveProposal; got != nil {
		t.Errorf("stale proposal shown as pending: %+v", got)
	}
}

func TestGateView_WaiveProposalRejectedOmitted(t *testing.T) {
	f := newReadsFixture(t)
	p := f.propose(t, f.row, proposalHeadA)
	f.au.seed(t, f.runID, CategoryConcernWaiveProposalRejected, map[string]any{
		"concern_id": f.row.ID.String(), "proposal_sequence": p.Sequence, "reason": "not convinced",
	})
	if got := openConcern(t, f.gateView(t), f.row.ID).WaiveProposal; got != nil {
		t.Errorf("rejected proposal shown as pending: %+v", got)
	}
}

// TestGateView_WaiveProposalFailedIntentStillPending is V2 on the read side: a
// linked concern_waived intent whose disposition failed (a concern_waive_failed
// naming it) leaves the proposal pending and confirmable.
func TestGateView_WaiveProposalFailedIntentStillPending(t *testing.T) {
	f := newReadsFixture(t)
	p := f.propose(t, f.row, proposalHeadA)
	intent := f.au.seed(t, f.runID, CategoryConcernWaived, map[string]any{
		"concern_id": f.row.ID.String(), waiveProposalKeySequence: p.Sequence,
	})
	f.au.seed(t, f.runID, CategoryConcernWaiveFailed, map[string]any{
		"concern_id": f.row.ID.String(), waiveFailedKeyIntentSequence: intent.Sequence,
	})
	got := openConcern(t, f.gateView(t), f.row.ID).WaiveProposal
	if got == nil || got.ProposalSequence != p.Sequence {
		t.Errorf("waive_proposal = %+v, want proposal %d still pending after a failed intent", got, p.Sequence)
	}
}

func TestGateView_WaiveProposalReadFailureRecordsGap(t *testing.T) {
	f := newReadsFixture(t)
	f.propose(t, f.row, proposalHeadA)
	f.au.listByCategoryErrCategory = CategoryConcernWaiveProposed
	resp := f.gateView(t)
	if got := openConcern(t, resp, f.row.ID).WaiveProposal; got != nil {
		t.Errorf("waive_proposal = %+v, want omitted on a read failure", got)
	}
	if !resp.HistoryIncomplete || !slices.Contains(resp.HistoryGaps, gateViewGapWaiveProposals) {
		t.Errorf("history_incomplete=%v history_gaps=%v, want waive_proposals", resp.HistoryIncomplete, resp.HistoryGaps)
	}
}

// TestGateView_WaiveProposalMarkerConcernCarriesBlock: a low
// diff_secrets_marker_suppressed row (ADR-084 A2, #5230) carries the block like
// any server_check row, and the diff_secrets_suppressed block is unchanged.
func TestGateView_WaiveProposalMarkerConcernCarriesBlock(t *testing.T) {
	f := newReadsFixture(t)
	rows, err := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.runID, StageID: f.stageID, StageKind: concern.StageKindImplement, Provenance: concern.ProvenanceServerCheck,
		OriginReviewSequence: 9,
		Concerns: []concern.RaisedConcern{{Severity: "low", Category: diffSecretsMarkerConcernCategory,
			Note: "allowlist marker suppressed a match", CheckKey: "diff_secrets_marker|fix/m.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	marker := rows[0]
	sum := diffsecrets.SuppressionSummary{Suppressed: []diffsecrets.SuppressionRecord{{
		Path: "fix/m.go", Lines: []int{2}, Pattern: "github-pat-classic", By: diffsecrets.ByMarker, RuleID: "fix-scope",
	}}}
	raw, _ := json.Marshal(diffSecretsDetectedPayload{Check: diffsecrets.CheckName, Groups: []diffSecretsGroupPayload{}, SuppressionSummary: sum})
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	f.au.seed(t, f.runID, diffSecretsDetectedCategory, fields)

	before, _ := json.Marshal(f.gateView(t).DiffSecretsSuppressed)
	p := f.propose(t, marker, proposalHeadA)
	resp := f.gateView(t)
	after, _ := json.Marshal(resp.DiffSecretsSuppressed)
	if resp.DiffSecretsSuppressed == nil || string(before) != string(after) {
		t.Errorf("diff_secrets_suppressed changed by a proposal:\nbefore %s\nafter  %s", before, after)
	}
	got := openConcern(t, resp, marker.ID).WaiveProposal
	if got == nil || got.ProposalSequence != p.Sequence || got.Category != diffSecretsMarkerConcernCategory || got.Severity != "low" {
		t.Errorf("marker waive_proposal = %+v, want proposal %d on the low marker row", got, p.Sequence)
	}
	if openConcern(t, resp, f.row.ID).WaiveProposal != nil {
		t.Error("the other server_check row carries the marker row's proposal")
	}
}

func TestGateViewCurrentHead_DelegatesToRecordedRunHead(t *testing.T) {
	f := newReadsFixture(t)
	f.au.seed(t, f.runID, CategoryBranchRebased, map[string]any{"already_up_to_date": true, "new_head_sha": "cccccccccccccccccccccccccccccccccccccccc"})
	f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
	ctx := context.Background()
	got, err := f.s.gateViewCurrentHead(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	want, err := recordedRunHead(ctx, f.au, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if got != proposalHeadB || got != want {
		t.Errorf("gateViewCurrentHead = %q, recordedRunHead = %q, want both %q", got, want, proposalHeadB)
	}
	f.au.listByCategoryErrCategory = "fixup_pushed"
	if _, err := f.s.gateViewCurrentHead(ctx, f.runID); err == nil {
		t.Error("gateViewCurrentHead swallowed a head read error")
	}
}

// getRunBody drives GET /v0/runs/{id} and returns the raw top-level keys.
func (f *readsFixture) getRunBody(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+f.runID.String(), nil)
	req.SetPathValue("run_id", f.runID.String())
	w := httptest.NewRecorder()
	f.s.handleGetRun(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET run = %d:\n%s", w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGetRun_WaiveProposalsPending(t *testing.T) {
	f := newReadsFixture(t)
	p := f.propose(t, f.row, proposalHeadA)
	raw, ok := f.getRunBody(t)["waive_proposals"]
	if !ok {
		t.Fatal("waive_proposals absent with a pending proposal")
	}
	var got []runWaiveProposalItem
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := runWaiveProposalItem{
		ConcernID: f.row.ID.String(), StageID: f.stageID.String(), ProposalSequence: p.Sequence,
		ProposedBy: readsProposer, Judgement: "tradeoff", HeadSHA: proposalHeadA,
	}
	if len(got) != 1 || got[0] != want {
		t.Errorf("waive_proposals = %+v, want [%+v]", got, want)
	}
	// The compact list carries no agent-authored text.
	if s := string(raw); strings.Contains(s, "documented placeholder") || strings.Contains(s, "README example") {
		t.Errorf("waive_proposals leaks agent-authored text: %s", s)
	}
}

func TestGetRun_WaiveProposalsOmittedWhenNone(t *testing.T) {
	t.Run("no proposal", func(t *testing.T) {
		f := newReadsFixture(t)
		if raw, ok := f.getRunBody(t)["waive_proposals"]; ok {
			t.Errorf("waive_proposals = %s, want omitted", raw)
		}
	})
	t.Run("stale proposal", func(t *testing.T) {
		f := newReadsFixture(t)
		f.propose(t, f.row, proposalHeadA)
		f.au.seed(t, f.runID, "fixup_pushed", map[string]any{"head_sha": proposalHeadB})
		if raw, ok := f.getRunBody(t)["waive_proposals"]; ok {
			t.Errorf("waive_proposals = %s, want omitted for a stale proposal", raw)
		}
	})
	t.Run("read failure", func(t *testing.T) {
		f := newReadsFixture(t)
		f.propose(t, f.row, proposalHeadA)
		f.au.listByCategoryErrCategory = CategoryConcernWaiveProposed
		if raw, ok := f.getRunBody(t)["waive_proposals"]; ok {
			t.Errorf("waive_proposals = %s, want omitted on a read failure", raw)
		}
	})
}

// TestWaiveProposal_PendingStillBlocksMergeGate: a proposal on a HIGH
// diff_secrets row leaves the row open, so the merge gate's blocking set still
// names it (the done-means "treated as still open").
func TestWaiveProposal_PendingStillBlocksMergeGate(t *testing.T) {
	f := newProposeFixture(t)
	if w := postPropose(t, f.s, f.row.ID.String(), validProposeBody(), withOperatorAgentAuth); w.Code != http.StatusCreated {
		t.Fatalf("propose = %d: %s", w.Code, w.Body.String())
	}
	ids, err := f.s.openBlockingConcernIDs(context.Background(), &run.Run{ID: f.runID, State: run.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, f.row.ID.String()) {
		t.Errorf("blocking concerns = %v, want the proposed-on row %s still blocking", ids, f.row.ID)
	}
}

// TestWaiveProposal_PendingMarkerConcernStillHoldsDelegatedSettlement: a
// proposal on a LOW marker row leaves it in ListOpenByRun, the set delegated
// settlement (evalGatesResolvedCIGreen) requires to be empty.
func TestWaiveProposal_PendingMarkerConcernStillHoldsDelegatedSettlement(t *testing.T) {
	f := newProposeFixture(t)
	rows, err := f.cr.InsertRaised(context.Background(), concern.InsertRaisedParams{
		RunID: f.runID, StageID: f.stageID, StageKind: concern.StageKindImplement, Provenance: concern.ProvenanceServerCheck,
		OriginReviewSequence: 9,
		Concerns: []concern.RaisedConcern{{Severity: "low", Category: diffSecretsMarkerConcernCategory,
			Note: "allowlist marker suppressed a match", CheckKey: "diff_secrets_marker|config/x.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := postPropose(t, f.s, rows[0].ID.String(), validProposeBody(), withOperatorAgentAuth); w.Code != http.StatusCreated {
		t.Fatalf("propose = %d: %s", w.Code, w.Body.String())
	}
	open, err := f.cr.ListOpenByRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range open {
		if c.ID == rows[0].ID {
			found = true
		}
	}
	if !found {
		t.Errorf("open rows = %d, want the proposed-on marker row %s still open", len(open), rows[0].ID)
	}
}

// waiveProposalContractNeedles are the strings both API contract docs must
// carry: the three routes, every refusal code, the three read fields and the
// refusal's agent_alternative pointer.
var waiveProposalContractNeedles = []string{
	"/v0/concerns/{concern_id}/waive-proposal",
	"/v0/concerns/{concern_id}/waive-proposal/confirm",
	"/v0/concerns/{concern_id}/waive-proposal/reject",
	"run_token_forbidden", "account_forbidden", "waive_proposal_contains_credential",
	"waive_proposal_head_unresolved", "waive_proposal_head_moved", "waive_proposal_exists",
	"waive_proposal_not_found", "waive_proposal_not_live", "waive_proposal_stale",
	"waive_proposal_atomic_append_unavailable", "concern_requires_human", "concern_requires_session",
	"waive_proposal", "waive_proposals", "agent_alternative", "intent_sequence",
}

func TestOpenAPI_WaiveProposalRoutesDocumented(t *testing.T) {
	doc := readRepoDoc(t, "docs/api/v0.openapi.yaml")
	for _, n := range waiveProposalContractNeedles {
		if !strings.Contains(doc, n) {
			t.Errorf("docs/api/v0.openapi.yaml does not carry %q (ADR-143 A1, #5229); update it with docs/api/v0.md and regenerate site/src/content/docs/reference/api.md", n)
		}
	}
}

func TestV0md_WaiveProposalRoutesDocumented(t *testing.T) {
	doc := readRepoDoc(t, "docs/api/v0.md")
	for _, n := range waiveProposalContractNeedles {
		if !strings.Contains(doc, n) {
			t.Errorf("docs/api/v0.md does not carry %q (ADR-143 A1, #5229); update it with docs/api/v0.openapi.yaml", n)
		}
	}
}

// TestADR143AmendmentA1_SettlesProposalContract pins that the landed A1 text
// settles the proposal contract the handlers implement.
func TestADR143AmendmentA1_SettlesProposalContract(t *testing.T) {
	doc := readRepoDoc(t, "docs/adr/143-human-only-actions-credential-kind.md")
	i := strings.Index(doc, "## Amendment A1: propose-and-confirm for server-check concerns")
	if i < 0 {
		t.Fatal("ADR-143 carries no Amendment A1 heading")
	}
	a1 := doc[i:]
	for _, n := range []string{
		"Ratification: Captain, 2026-11-18 (issue #5229 comment)",
		"clears nothing", "session", "run_token_forbidden", "head", "recorded run head",
		"concern_waive_proposal_dropped", "concern_waive_proposal_rejected",
		"diff_secrets_marker_suppressed", "ADR-148 D7", "GuardedChainAppender", "best effort",
	} {
		if !strings.Contains(a1, n) {
			t.Errorf("ADR-143 Amendment A1 does not carry %q", n)
		}
	}
}

// TestReviewDoc_WaiveProposalContract pins review.md as the prose contract
// site for the flow (the other doc sites are follow-ups, X2).
func TestReviewDoc_WaiveProposalContract(t *testing.T) {
	doc := readRepoDoc(t, "backend/internal/server/docs/review.md")
	for _, n := range []string{
		"waive-proposal/confirm", "waive-proposal/reject", "GuardedChainAppender", "intent_sequence",
		"concern_waive_proposal_dropped", "diff_secrets_marker_suppressed", "recorded run head",
		"concern_requires_session", "Intent-without-outcome residual",
	} {
		if !strings.Contains(doc, n) {
			t.Errorf("backend/internal/server/docs/review.md does not carry %q (ADR-143 A1, #5229)", n)
		}
	}
}
