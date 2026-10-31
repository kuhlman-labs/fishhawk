package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// label_concern_test.go pins POST /v0/concerns/{concern_id}/label (ADR-117 D1
// phase 3, E94.8 / #4841). Every refusal asserts the status and rule, the
// concern UNCHANGED on re-read and ZERO concern_labelled rows: the handler's
// own guards run before the intent append, so a deleted guard that the fake
// store still catches is observable as an intent row on the chain.

// labelHuman is a human operator token carrying the waive scope pair's
// write:stages half.
func labelHuman() Identity {
	return Identity{Subject: "github:alice", TokenID: "tok-alice", Scopes: []string{"write:stages"}}
}

// labelServer wires the run, audit and concern fakes (the gate-view server) and
// seeds a run on the non-Fishhawk repository acme/widgets (ADR-110).
func labelServer(t *testing.T) (*Server, *fakeRepo, *auditFake, *fakeConcernRepo, uuid.UUID) {
	t.Helper()
	s, repo, au, cr := gateViewServer(t)
	return s, repo, au, cr, seedLabelRun(t, repo)
}

func seedLabelRun(t *testing.T, repo *fakeRepo) uuid.UUID {
	t.Helper()
	got, err := repo.CreateRun(context.Background(), run.CreateRunParams{
		Repo: "acme/widgets", WorkflowID: "feature_change", WorkflowSHA: "s", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return got.ID
}

// seedLabelConcern inserts one raised implement concern at origin sequence seq
// (the fake leaves CreatedAt zero, so distinct sequences fix the ledger order).
func seedLabelConcern(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID, seq int64) *concern.Concern {
	t.Helper()
	return seedConcernRow(t, cr, runID, uuid.New(), concern.StageKindImplement, seq, "concern at sequence")
}

func postLabel(t *testing.T, s *Server, concernID, raw string, id Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+concernID+"/label", strings.NewReader(raw))
	req.SetPathValue("concern_id", concernID)
	req = injectIdentity(req, id)
	w := httptest.NewRecorder()
	s.handleLabelConcern(w, req)
	return w
}

func labelBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return string(raw)
}

type labelErrBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func decodeLabelErr(t *testing.T, w *httptest.ResponseRecorder) labelErrBody {
	t.Helper()
	var b labelErrBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
	}
	return b
}

func decodeLabelOK(t *testing.T, w *httptest.ResponseRecorder) labelConcernResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp labelConcernResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return resp
}

// labelPayloads returns the decoded payloads of every appended entry of category.
func labelPayloads(t *testing.T, au *auditFake, category string) []map[string]any {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	var out []map[string]any
	for _, e := range au.appended {
		if e.Category != category {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode %s payload: %v", category, err)
		}
		out = append(out, m)
	}
	return out
}

// labelJudgeDirect labels a row through the fake store, bypassing the handler,
// to set up a fixture judgement.
func labelJudgeDirect(t *testing.T, cr *fakeConcernRepo, row *concern.Concern, j concern.Judgement) {
	t.Helper()
	if _, err := cr.ApplyLabel(context.Background(), row.ID, concern.Label{
		Judgement: j, ActorKind: "user", PriorJudgement: row.Judgement, PriorDuplicateOf: row.DuplicateOf,
	}); err != nil {
		t.Fatalf("fixture label %s: %v", j, err)
	}
}

// TestLabelConcern_RequiresHuman: every agent subject family is refused 403
// label_requires_human before anything is read or written, while a human token
// carrying the SAME scope commits — so the agent rows differ only in subject
// and the refusal is the isAgentSubject guard alone.
//
// Counterfactual C8: delete the handler's isAgentSubject block and every agent
// row commits (200, a concern_labelled row, the judgement written). LOW 3:
// delete isAgentSubject's firstofficer.IsSeatSubject branch and the
// first-officer row commits.
func TestLabelConcern_RequiresHuman(t *testing.T) {
	cases := []struct {
		name    string
		subject func(runID uuid.UUID) string
	}{
		{"operator-agent token", func(uuid.UUID) string { return "operator-agent/v1" }},
		{"run-bound mcp token on its own run", func(runID uuid.UUID) string { return "mcp:run:" + runID.String() }},
		{"first-officer seat", func(runID uuid.UUID) string { return "first_officer/run/" + runID.String() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, au, cr, runID := labelServer(t)
			row := seedLabelConcern(t, cr, runID, 1)
			before := *row
			id := Identity{Subject: tc.subject(runID), TokenID: "tok-agent", Scopes: []string{"write:stages", "write:fixups"}}
			w := postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, id)
			if w.Code != http.StatusForbidden || decodeLabelErr(t, w).Error.Code != errCodeLabelRequiresHuman {
				t.Fatalf("status = %d, want 403 %s:\n%s", w.Code, errCodeLabelRequiresHuman, w.Body.String())
			}
			if !reflect.DeepEqual(before, *row) {
				t.Errorf("agent label changed the row:\nbefore %+v\nafter  %+v", before, *row)
			}
			if n := len(labelPayloads(t, au, CategoryConcernLabelled)); n != 0 {
				t.Errorf("concern_labelled rows = %d, want 0", n)
			}
		})
	}
	t.Run("human control commits", func(t *testing.T) {
		s, _, _, cr, runID := labelServer(t)
		row := seedLabelConcern(t, cr, runID, 1)
		if got := decodeLabelOK(t, postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, labelHuman())); got.Judgement != "defect" {
			t.Errorf("human judgement = %q, want defect", got.Judgement)
		}
	})
}

// TestLabelConcern_AuthAndConfigLadder pins the rungs ahead of the human-only
// guard: anonymous 401, a token with neither write:stages nor write:fixups 403,
// write:fixups alone admitted, and an unwired store 503.
func TestLabelConcern_AuthAndConfigLadder(t *testing.T) {
	s, _, au, cr, runID := labelServer(t)
	row := seedLabelConcern(t, cr, runID, 1)

	if w := postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, Identity{}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	noScope := Identity{Subject: "github:alice", TokenID: "tok", Scopes: []string{"read:runs"}}
	if w := postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, noScope); w.Code != http.StatusForbidden ||
		decodeLabelErr(t, w).Error.Code != "insufficient_scope" {
		t.Errorf("unscoped status = %d, want 403 insufficient_scope:\n%s", w.Code, w.Body.String())
	}
	if n := len(labelPayloads(t, au, CategoryConcernLabelled)); n != 0 {
		t.Fatalf("concern_labelled rows after refusals = %d, want 0", n)
	}
	fixups := Identity{Subject: "github:alice", TokenID: "tok", Scopes: []string{"write:fixups"}}
	decodeLabelOK(t, postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, fixups))

	bare := New(Config{Addr: "127.0.0.1:0"})
	if w := postLabel(t, bare, row.ID.String(), `{"judgement":"defect"}`, labelHuman()); w.Code != http.StatusServiceUnavailable ||
		decodeLabelErr(t, w).Error.Code != "concern_store_unconfigured" {
		t.Errorf("unconfigured status = %d, want 503 concern_store_unconfigured:\n%s", w.Code, w.Body.String())
	}
}

// TestLabelConcern_RefusalsWriteNothing is the refusal table. The labelled
// concern C sits at origin sequence 2 of run R, beside an EARLIER same-run
// concern A (sequence 1), a LATER one L (sequence 3) and a concern F of ANOTHER
// run at sequence 0. The duplicate_of rows that a deleted handler guard would
// let through point at A, a target the store would otherwise ACCEPT, so the
// guard is the only thing between the request and an intent row.
//
// Counterfactual C9: delete the handler's exclusivity check — the store's
// Label.Validate still refuses, but after the intent append, so the
// zero-intent-row assertion goes RED. C10: delete the handler's
// CheckDuplicateTarget call — the fake store refuses the cross-run row after
// the intent append, RED the same way. C6 (label route): the 4001-byte
// tradeoff_cost row.
func TestLabelConcern_RefusalsWriteNothing(t *testing.T) {
	s, repo, au, cr, runID := labelServer(t)
	a := seedLabelConcern(t, cr, runID, 1)
	c := seedLabelConcern(t, cr, runID, 2)
	l := seedLabelConcern(t, cr, runID, 3)
	f := seedLabelConcern(t, cr, seedLabelRun(t, repo), 0)

	cases := []struct {
		name      string
		concernID string
		body      string
		status    int
		code      string
		field     string
		rule      string
	}{
		{"judgement with duplicate_of", c.ID.String(), labelBody(t, map[string]any{"judgement": "defect", "duplicate_of": a.ID.String()}),
			400, "validation_failed", "duplicate_of", concern.JudgementRuleDuplicateOfExcludesJudgement},
		{"tradeoff_cost with duplicate_of", c.ID.String(), labelBody(t, map[string]any{"tradeoff_cost": "slower", "duplicate_of": a.ID.String()}),
			400, "validation_failed", "tradeoff_cost", concern.JudgementRuleDuplicateOfExcludesJudgement},
		{"neither judgement nor duplicate_of", c.ID.String(), `{}`,
			400, "validation_failed", "judgement", concern.JudgementRuleRequired},
		{"unlabelled judgement", c.ID.String(), `{"judgement":"unlabelled"}`,
			400, "validation_failed", "judgement", concern.JudgementRuleUnlabelled},
		{"unknown judgement", c.ID.String(), `{"judgement":"bogus"}`,
			400, "validation_failed", "judgement", concern.JudgementRuleUnknown},
		{"tradeoff without a cost", c.ID.String(), `{"judgement":"tradeoff"}`,
			400, "validation_failed", "tradeoff_cost", concern.JudgementRuleTradeoffCostRequired},
		{"cost without a tradeoff", c.ID.String(), `{"judgement":"defect","tradeoff_cost":"x"}`,
			400, "validation_failed", "tradeoff_cost", concern.JudgementRuleTradeoffCostWithoutTradeoff},
		{"tradeoff_cost over the cap", c.ID.String(), labelBody(t, map[string]any{"judgement": "tradeoff", "tradeoff_cost": strings.Repeat("x", maxOperatorConcernBytes+1)}),
			400, "validation_failed", "tradeoff_cost", concern.JudgementRuleTradeoffCostTooLong},
		{"malformed duplicate_of", c.ID.String(), `{"duplicate_of":"not-a-uuid"}`,
			400, "validation_failed", "duplicate_of", ""},
		{"malformed concern_id", "not-a-uuid", `{"judgement":"defect"}`,
			400, "validation_failed", "concern_id", ""},
		{"malformed body", c.ID.String(), `{"judgement":`,
			400, "validation_failed", "", ""},
		{"duplicate_of target missing", c.ID.String(), labelBody(t, map[string]any{"duplicate_of": uuid.NewString()}),
			400, "validation_failed", "duplicate_of", concern.JudgementRuleDuplicateOfNotFound},
		{"duplicate_of target in another run", c.ID.String(), labelBody(t, map[string]any{"duplicate_of": f.ID.String()}),
			400, "validation_failed", "duplicate_of", concern.JudgementRuleDuplicateOfNotFound},
		{"duplicate_of self", c.ID.String(), labelBody(t, map[string]any{"duplicate_of": c.ID.String()}),
			400, "validation_failed", "duplicate_of", concern.JudgementRuleDuplicateOfNotEarlier},
		{"duplicate_of a later concern", c.ID.String(), labelBody(t, map[string]any{"duplicate_of": l.ID.String()}),
			400, "validation_failed", "duplicate_of", concern.JudgementRuleDuplicateOfNotEarlier},
		{"concern not found", uuid.NewString(), `{"judgement":"defect"}`,
			404, "concern_not_found", "", ""},
	}
	snapshot := func() []concern.Concern {
		out := []concern.Concern{}
		for _, r := range []*concern.Concern{a, c, l, f} {
			out = append(out, *r)
		}
		return out
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshot()
			w := postLabel(t, s, tc.concernID, tc.body, labelHuman())
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, tc.status, w.Body.String())
			}
			b := decodeLabelErr(t, w)
			if b.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", b.Error.Code, tc.code)
			}
			if tc.field != "" && b.Error.Details["field"] != tc.field {
				t.Errorf("details.field = %v, want %q", b.Error.Details["field"], tc.field)
			}
			if tc.rule != "" && b.Error.Details["rule"] != tc.rule {
				t.Errorf("details.rule = %v, want %q:\n%s", b.Error.Details["rule"], tc.rule, w.Body.String())
			}
			if after := snapshot(); !reflect.DeepEqual(before, after) {
				t.Errorf("refusal changed a row:\nbefore %+v\nafter  %+v", before, after)
			}
			if n := len(labelPayloads(t, au, CategoryConcernLabelled)); n != 0 {
				t.Errorf("concern_labelled rows = %d, want 0 (a refusal must land before the intent append)", n)
			}
		})
	}
}

// TestLabelConcern_MissingAndForeignTargetIndistinguishable pins approval
// condition M1 at the REST layer: a missing duplicate_of target and a target in
// ANOTHER run answer byte-identical bodies under one rule, and no byte of the
// foreign run's id appears. The foreign target sits at a LOWER sequence, so the
// ordering rule would accept it and the run check is what refuses it.
//
// Counterfactual: make the handler name the target's run in the refusal and
// the identity and no-foreign-id assertions go RED.
func TestLabelConcern_MissingAndForeignTargetIndistinguishable(t *testing.T) {
	s, repo, _, cr, runID := labelServer(t)
	c := seedLabelConcern(t, cr, runID, 2)
	foreignRun := seedLabelRun(t, repo)
	f := seedLabelConcern(t, cr, foreignRun, 0)

	missing := postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": uuid.NewString()}), labelHuman())
	foreign := postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": f.ID.String()}), labelHuman())
	if missing.Code != http.StatusBadRequest || foreign.Code != http.StatusBadRequest {
		t.Fatalf("statuses = %d / %d, want 400 / 400", missing.Code, foreign.Code)
	}
	if missing.Body.String() != foreign.Body.String() {
		t.Errorf("missing and foreign targets are distinguishable:\nmissing %s\nforeign %s", missing.Body.String(), foreign.Body.String())
	}
	for _, leak := range []string{foreignRun.String(), f.ID.String()} {
		if strings.Contains(foreign.Body.String(), leak) {
			t.Errorf("foreign-target refusal names %s:\n%s", leak, foreign.Body.String())
		}
	}
	if got := decodeLabelErr(t, foreign).Error.Details["run_id"]; got != runID.String() {
		t.Errorf("details.run_id = %v, want the labelled concern's own run %s", got, runID)
	}
}

// TestLabelConcern_Backfill pins the backfill import: a WAIVED row whose stored
// reason carries an interim prefix supplies the judgement when backfill is set,
// and nowhere else.
//
// Counterfactual C11: hard-code Backfill:false in the handler's Label and the
// import row reads judgement_backfill false (RED). C12: drop the
// `row.State == concern.StateWaived` condition and the raised-row case derives
// no_defect and commits (RED).
func TestLabelConcern_Backfill(t *testing.T) {
	waived := func(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID) *concern.Concern {
		row := seedLabelConcern(t, cr, runID, 1)
		row.State = concern.StateWaived
		row.StateReason = "false_positive: the retry is bounded upstream"
		return row
	}

	t.Run("waived row imports its prefix", func(t *testing.T) {
		s, _, au, cr, runID := labelServer(t)
		row := waived(t, cr, runID)
		got := decodeLabelOK(t, postLabel(t, s, row.ID.String(), `{"backfill":true}`, labelHuman()))
		if got.Judgement != string(concern.JudgementNoDefect) || !got.JudgementBackfill {
			t.Errorf("response judgement = %q backfill = %v, want no_defect true", got.Judgement, got.JudgementBackfill)
		}
		if row.Judgement != concern.JudgementNoDefect || !row.JudgementBackfill {
			t.Errorf("stored judgement = %q backfill = %v, want no_defect true", row.Judgement, row.JudgementBackfill)
		}
		if row.State != concern.StateWaived {
			t.Errorf("state = %q, want waived (a label is not a lifecycle event)", row.State)
		}
		p := labelPayloads(t, au, CategoryConcernLabelled)
		if len(p) != 1 || p[0]["backfill"] != true || p[0]["judgement"] != string(concern.JudgementNoDefect) {
			t.Errorf("concern_labelled payloads = %v, want one with backfill true and judgement no_defect", p)
		}
	})

	refusals := []struct {
		name  string
		setup func(*testing.T, *fakeConcernRepo, uuid.UUID) *concern.Concern
		body  string
		rule  string
	}{
		{"waived row without backfill", waived, `{}`, concern.JudgementRuleRequired},
		{"raised row with a prefixed reason", func(t *testing.T, cr *fakeConcernRepo, runID uuid.UUID) *concern.Concern {
			row := seedLabelConcern(t, cr, runID, 1)
			row.StateReason = "false_positive: seeded on an open row"
			return row
		}, `{"backfill":true}`, concern.JudgementRuleRequired},
		{"backfill contradicting an explicit judgement", waived, `{"backfill":true,"judgement":"defect"}`, concern.JudgementRuleConflictsReasonPrefix},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			s, _, au, cr, runID := labelServer(t)
			row := tc.setup(t, cr, runID)
			before := *row
			w := postLabel(t, s, row.ID.String(), tc.body, labelHuman())
			if w.Code != http.StatusBadRequest || decodeLabelErr(t, w).Error.Details["rule"] != tc.rule {
				t.Fatalf("status = %d, want 400 rule %s:\n%s", w.Code, tc.rule, w.Body.String())
			}
			if !reflect.DeepEqual(before, *row) {
				t.Errorf("refusal changed the row:\nbefore %+v\nafter  %+v", before, *row)
			}
			if n := len(labelPayloads(t, au, CategoryConcernLabelled)); n != 0 {
				t.Errorf("concern_labelled rows = %d, want 0", n)
			}
		})
	}
}

// TestLabelConcern_RelabelIsVersioned: two relabels each append their own
// concern_labelled row, the second carrying the first's judgement as its
// prior; neither touches state, state_reason or outcome.
func TestLabelConcern_RelabelIsVersioned(t *testing.T) {
	s, _, au, cr, runID := labelServer(t)
	row := seedLabelConcern(t, cr, runID, 1)
	row.Outcome = concern.OutcomeOpenAtMerge
	state, reason, outcome := row.State, row.StateReason, row.Outcome

	decodeLabelOK(t, postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, labelHuman()))
	got := decodeLabelOK(t, postLabel(t, s, row.ID.String(), `{"judgement":"tradeoff","tradeoff_cost":"one extra round trip"}`, labelHuman()))
	if got.Judgement != "tradeoff" || got.TradeoffCost != "one extra round trip" || got.JudgementActorKind != "user" {
		t.Errorf("response = %+v, want tradeoff with its cost by a user", got.concernJudgementView)
	}
	if row.State != state || row.StateReason != reason || row.Outcome != outcome {
		t.Errorf("label moved lifecycle columns: state %q reason %q outcome %q", row.State, row.StateReason, row.Outcome)
	}
	p := labelPayloads(t, au, CategoryConcernLabelled)
	if len(p) != 2 {
		t.Fatalf("concern_labelled rows = %d, want 2", len(p))
	}
	if p[0]["prior_judgement"] != "unlabelled" || p[0]["judgement"] != "defect" {
		t.Errorf("first payload = %v, want prior unlabelled -> defect", p[0])
	}
	if p[1]["prior_judgement"] != "defect" || p[1]["judgement"] != "tradeoff" || p[1]["tradeoff_cost"] != "one extra round trip" {
		t.Errorf("second payload = %v, want prior defect -> tradeoff with its cost", p[1])
	}
	if _, has := p[0]["tradeoff_cost"]; has {
		t.Errorf("first payload carries tradeoff_cost with no cost: %v", p[0])
	}
}

// TestLabelConcern_DuplicateInheritsAndJudgementClears: a duplicate's response
// carries its root's judgement and the duplicate_of outcome, and a later
// judgement label clears the link.
func TestLabelConcern_DuplicateInheritsAndJudgementClears(t *testing.T) {
	s, _, au, cr, runID := labelServer(t)
	a := seedLabelConcern(t, cr, runID, 1)
	c := seedLabelConcern(t, cr, runID, 2)
	labelJudgeDirect(t, cr, a, concern.JudgementDefect)

	dup := decodeLabelOK(t, postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": a.ID.String()}), labelHuman()))
	if dup.Judgement != "defect" || dup.JudgementInheritedFrom == nil || *dup.JudgementInheritedFrom != a.ID {
		t.Errorf("duplicate response judgement = %q inherited_from = %v, want defect from %s", dup.Judgement, dup.JudgementInheritedFrom, a.ID)
	}
	if dup.Outcome != concern.DuplicateOutcomePrefix+a.ID.String() || dup.DuplicateOf == nil || *dup.DuplicateOf != a.ID {
		t.Errorf("duplicate response outcome = %q duplicate_of = %v, want the duplicate_of:%s outcome", dup.Outcome, dup.DuplicateOf, a.ID)
	}
	if c.Judgement != concern.JudgementUnlabelled {
		t.Errorf("stored duplicate judgement = %q, want unlabelled (it inherits)", c.Judgement)
	}

	cleared := decodeLabelOK(t, postLabel(t, s, c.ID.String(), `{"judgement":"preference"}`, labelHuman()))
	if cleared.Judgement != "preference" || cleared.DuplicateOf != nil || cleared.JudgementInheritedFrom != nil {
		t.Errorf("cleared response = %+v, want its own preference with no link", cleared.concernJudgementView)
	}
	p := labelPayloads(t, au, CategoryConcernLabelled)
	if len(p) != 2 || p[0]["duplicate_of"] != a.ID.String() || p[1]["prior_duplicate_of"] != a.ID.String() {
		t.Errorf("payloads = %v, want the link then its clearing naming the prior link", p)
	}
	if _, has := p[0]["judgement"]; has {
		t.Errorf("duplicate payload carries a judgement: %v", p[0])
	}
}

// labelGateViewJudgements reads GET /v0/runs/{id}/gate-view and returns every
// open and settled concern's judgement view by id.
func labelGateViewJudgements(t *testing.T, s *Server, runID uuid.UUID) map[uuid.UUID]concernJudgementView {
	t.Helper()
	gv := decodeGateView(t, getGateView(t, s, runID, ""))
	out := map[uuid.UUID]concernJudgementView{}
	for _, c := range gv.Open {
		out[c.ID] = c.concernJudgementView
	}
	for _, c := range gv.Settled {
		out[c.ID] = c.concernJudgementView
	}
	return out
}

// TestLabelConcern_ChainReadsRootThroughGateView: C -> B -> A, each link written
// through the verb, reads A's judgement on C at the gate view.
func TestLabelConcern_ChainReadsRootThroughGateView(t *testing.T) {
	s, _, _, cr, runID := labelServer(t)
	a := seedLabelConcern(t, cr, runID, 1)
	b := seedLabelConcern(t, cr, runID, 2)
	c := seedLabelConcern(t, cr, runID, 3)
	labelJudgeDirect(t, cr, a, concern.JudgementDefectPreExisting)
	decodeLabelOK(t, postLabel(t, s, b.ID.String(), labelBody(t, map[string]any{"duplicate_of": a.ID.String()}), labelHuman()))
	decodeLabelOK(t, postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": b.ID.String()}), labelHuman()))

	got := labelGateViewJudgements(t, s, runID)[c.ID]
	if got.Judgement != string(concern.JudgementDefectPreExisting) || got.JudgementInheritedFrom == nil || *got.JudgementInheritedFrom != a.ID {
		t.Errorf("C at the gate view = %+v, want defect_pre_existing inherited from %s", got, a.ID)
	}
}

// TestLabelConcern_BrokenChainsReadUnlabelled: a cycle and a dangling link can
// only be seeded below the API (the verb refuses both); seeded on the fake's
// rows, both read unlabelled with no judgement_inherited_from at the gate view
// AND the run concern listing, whatever their own stored judgement.
func TestLabelConcern_BrokenChainsReadUnlabelled(t *testing.T) {
	s, _, _, cr, runID := labelServer(t)
	x := seedLabelConcern(t, cr, runID, 1)
	y := seedLabelConcern(t, cr, runID, 2)
	z := seedLabelConcern(t, cr, runID, 3)
	xID, yID, dangling := x.ID, y.ID, uuid.New()
	x.DuplicateOf, x.Judgement = &yID, concern.JudgementDefect
	y.DuplicateOf, y.Judgement = &xID, concern.JudgementDefect
	z.DuplicateOf, z.Judgement = &dangling, concern.JudgementDefect

	gv := labelGateViewJudgements(t, s, runID)
	list := decodeRunConcerns(t, callRunConcerns(s, runID.String(), "state=all", gateViewReadIdentity()))
	listed := map[string]concernJudgementView{}
	for _, it := range list.Items {
		listed[it.ID] = it.concernJudgementView
	}
	for _, id := range []uuid.UUID{xID, yID, z.ID} {
		for surface, v := range map[string]concernJudgementView{"gate-view": gv[id], "concerns": listed[id.String()]} {
			if v.Judgement != "unlabelled" || v.JudgementInheritedFrom != nil {
				t.Errorf("%s %s = %+v, want unlabelled with no judgement_inherited_from", surface, id, v)
			}
		}
	}
}

// TestLabelConcern_HumanWaiveOnDuplicateKeepsInheritance pins approval
// condition M2 at the REST layer: a human waive of a duplicate-linked concern
// records its judgement on the row and the concern_waived payload, but the
// EFFECTIVE judgement stays the root's until the concern is relabelled.
func TestLabelConcern_HumanWaiveOnDuplicateKeepsInheritance(t *testing.T) {
	s, _, au, cr, runID := labelServer(t)
	a := seedLabelConcern(t, cr, runID, 1)
	c := seedLabelConcern(t, cr, runID, 2)
	labelJudgeDirect(t, cr, a, concern.JudgementDefect)
	decodeLabelOK(t, postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": a.ID.String()}), labelHuman()))

	if w := postWaive(t, s, c.ID.String(), waiveConcernRequest{Reason: "same as A", Judgement: string(concern.JudgementNoDefect)}); w.Code != http.StatusOK {
		t.Fatalf("waive status = %d:\n%s", w.Code, w.Body.String())
	}
	if p := labelPayloads(t, au, CategoryConcernWaived); len(p) != 1 || p[0]["judgement"] != string(concern.JudgementNoDefect) {
		t.Errorf("concern_waived payloads = %v, want the waive's no_defect recorded", p)
	}
	got := labelGateViewJudgements(t, s, runID)[c.ID]
	if got.Judgement != string(concern.JudgementDefect) || got.JudgementInheritedFrom == nil || *got.JudgementInheritedFrom != a.ID {
		t.Errorf("waived duplicate reads %+v, want the root's defect inherited from %s", got, a.ID)
	}
}

// TestLabelConcern_AuditAppendFailureWritesNothing: the intent append failing
// is 500 audit_append_failed with the row unchanged and no corrective row.
//
// Counterfactual C13: move ApplyLabel ahead of the append and the re-read row
// carries the new judgement (RED).
func TestLabelConcern_AuditAppendFailureWritesNothing(t *testing.T) {
	s, _, au, cr, runID := labelServer(t)
	row := seedLabelConcern(t, cr, runID, 1)
	before := *row
	au.appendErrCategory = CategoryConcernLabelled

	w := postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, labelHuman())
	if w.Code != http.StatusInternalServerError || decodeLabelErr(t, w).Error.Code != "audit_append_failed" {
		t.Fatalf("status = %d, want 500 audit_append_failed:\n%s", w.Code, w.Body.String())
	}
	if !reflect.DeepEqual(before, *row) {
		t.Errorf("a failed intent append changed the row:\nbefore %+v\nafter  %+v", before, *row)
	}
	if n := len(labelPayloads(t, au, CategoryConcernLabelFailed)); n != 0 {
		t.Errorf("concern_label_failed rows = %d, want 0 (no intent to correct)", n)
	}
}

// TestLabelConcern_StoreFailureAppendsCorrective: a write failing after the
// intent append is 500 plus one concern_label_failed row naming the intended
// label.
//
// Counterfactual C14: delete the corrective append and this goes RED.
func TestLabelConcern_StoreFailureAppendsCorrective(t *testing.T) {
	s, _, au, cr, runID := labelServer(t)
	row := seedLabelConcern(t, cr, runID, 1)
	cr.applyLabelErr = errors.New("store unavailable")

	w := postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, labelHuman())
	if w.Code != http.StatusInternalServerError || decodeLabelErr(t, w).Error.Code != "internal_error" {
		t.Fatalf("status = %d, want 500 internal_error:\n%s", w.Code, w.Body.String())
	}
	if n := len(labelPayloads(t, au, CategoryConcernLabelled)); n != 1 {
		t.Errorf("concern_labelled rows = %d, want 1", n)
	}
	failed := labelPayloads(t, au, CategoryConcernLabelFailed)
	if len(failed) != 1 || failed[0]["intended_judgement"] != "defect" || failed[0]["error"] != "store unavailable" {
		t.Errorf("concern_label_failed payloads = %v, want one naming intended defect and the error", failed)
	}
}

// interveningLabelRepo relabels the concern through the fake store between the
// handler's read and its ApplyLabel call: a concurrent relabel.
type interveningLabelRepo struct {
	*fakeConcernRepo
	intervene func()
}

func (r *interveningLabelRepo) ApplyLabel(ctx context.Context, id uuid.UUID, l concern.Label) (*concern.Concern, error) {
	r.intervene()
	return r.fakeConcernRepo.ApplyLabel(ctx, id, l)
}

// TestLabelConcern_ConcurrentRelabelConflicts: a relabel landing between the
// handler's read and its write is 409 concern_label_conflict plus a
// concern_label_failed row, and the intervening judgement is kept. The handler
// must snapshot the prior it read: the fake hands out its stored pointer, so a
// prior read late off the row would absorb the intervening write.
func TestLabelConcern_ConcurrentRelabelConflicts(t *testing.T) {
	_, repo, au, cr, runID := labelServer(t)
	row := seedLabelConcern(t, cr, runID, 1)
	wrapped := &interveningLabelRepo{fakeConcernRepo: cr, intervene: func() {
		labelJudgeDirect(t, cr, row, concern.JudgementStale)
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: wrapped})

	w := postLabel(t, s, row.ID.String(), `{"judgement":"defect"}`, labelHuman())
	if w.Code != http.StatusConflict || decodeLabelErr(t, w).Error.Code != errCodeConcernLabelConflict {
		t.Fatalf("status = %d, want 409 %s:\n%s", w.Code, errCodeConcernLabelConflict, w.Body.String())
	}
	if row.Judgement != concern.JudgementStale {
		t.Errorf("judgement = %q, want the intervening stale kept", row.Judgement)
	}
	if n := len(labelPayloads(t, au, CategoryConcernLabelFailed)); n != 1 {
		t.Errorf("concern_label_failed rows = %d, want 1", n)
	}
}

// TestLabelConcern_VanishedTargetIsTheDuplicateRule pins approval LOW 2: a
// duplicate_of target that disappears between the handler's pre-check and the
// store write is the store's distinct sentinel, answered as the 400
// duplicate_of_not_found rule — never the labelled concern's 404.
//
// Counterfactual: delete the handler's JudgementError branch in the write-error
// mapping and the response is 500 (RED).
func TestLabelConcern_VanishedTargetIsTheDuplicateRule(t *testing.T) {
	_, repo, au, cr, runID := labelServer(t)
	a := seedLabelConcern(t, cr, runID, 1)
	c := seedLabelConcern(t, cr, runID, 2)
	wrapped := &interveningLabelRepo{fakeConcernRepo: cr, intervene: func() {
		cr.mu.Lock()
		defer cr.mu.Unlock()
		kept := cr.rows[:0]
		for _, r := range cr.rows {
			if r.ID != a.ID {
				kept = append(kept, r)
			}
		}
		cr.rows = kept
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: wrapped})

	w := postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": a.ID.String()}), labelHuman())
	if w.Code != http.StatusBadRequest || decodeLabelErr(t, w).Error.Details["rule"] != concern.JudgementRuleDuplicateOfNotFound {
		t.Fatalf("status = %d, want 400 %s:\n%s", w.Code, concern.JudgementRuleDuplicateOfNotFound, w.Body.String())
	}
	if c.DuplicateOf != nil {
		t.Errorf("duplicate_of = %v, want still unset", c.DuplicateOf)
	}
	if n := len(labelPayloads(t, au, CategoryConcernLabelFailed)); n != 1 {
		t.Errorf("concern_label_failed rows = %d, want 1", n)
	}
}

// targetReadFailRepo fails GetByIDs for one id with a non-NotFound error.
type targetReadFailRepo struct {
	*fakeConcernRepo
	failID uuid.UUID
}

func (r *targetReadFailRepo) GetByIDs(ctx context.Context, ids []uuid.UUID) ([]*concern.Concern, error) {
	for _, id := range ids {
		if id == r.failID {
			return nil, errors.New("target read failed")
		}
	}
	return r.fakeConcernRepo.GetByIDs(ctx, ids)
}

// TestLabelConcern_ReadFailures: a failed concern read or duplicate_of target
// read (other than not-found) is 500 with nothing written.
func TestLabelConcern_ReadFailures(t *testing.T) {
	_, repo, au, cr, runID := labelServer(t)
	a := seedLabelConcern(t, cr, runID, 1)
	c := seedLabelConcern(t, cr, runID, 2)
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, ConcernRepo: &targetReadFailRepo{fakeConcernRepo: cr, failID: a.ID}})

	if w := postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": a.ID.String()}), labelHuman()); w.Code != http.StatusInternalServerError {
		t.Errorf("target read failure status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if w := postLabel(t, s, a.ID.String(), `{"judgement":"defect"}`, labelHuman()); w.Code != http.StatusInternalServerError {
		t.Errorf("concern read failure status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if c.DuplicateOf != nil || a.Judgement != "" {
		t.Errorf("a read failure wrote: duplicate_of %v judgement %q", c.DuplicateOf, a.Judgement)
	}
	if n := len(labelPayloads(t, au, CategoryConcernLabelled)); n != 0 {
		t.Errorf("concern_labelled rows = %d, want 0", n)
	}
}

// TestLabelConcern_ListFailureRendersOwnJudgement: a ListByRun failure after
// the write still answers 200, rendering the row's OWN judgement (a duplicate
// therefore reads unlabelled with no inherited_from), because the label is
// already durable.
func TestLabelConcern_ListFailureRendersOwnJudgement(t *testing.T) {
	s, _, _, cr, runID := labelServer(t)
	a := seedLabelConcern(t, cr, runID, 1)
	c := seedLabelConcern(t, cr, runID, 2)
	labelJudgeDirect(t, cr, a, concern.JudgementDefect)
	cr.listErr = errors.New("list failed")

	got := decodeLabelOK(t, postLabel(t, s, c.ID.String(), labelBody(t, map[string]any{"duplicate_of": a.ID.String()}), labelHuman()))
	if got.Judgement != "unlabelled" || got.JudgementInheritedFrom != nil || got.DuplicateOf == nil {
		t.Errorf("degraded response = %+v, want its own unlabelled with the link and no inherited_from", got.concernJudgementView)
	}
	if c.DuplicateOf == nil || *c.DuplicateOf != a.ID {
		t.Errorf("stored duplicate_of = %v, want %s", c.DuplicateOf, a.ID)
	}
}

// labelDocumentedCodes is every code handleLabelConcern answers.
var labelDocumentedCodes = []string{
	"authentication_required", "insufficient_scope", errCodeLabelRequiresHuman, "concern_store_unconfigured",
	"validation_failed", "concern_not_found", "audit_append_failed", errCodeConcernLabelConflict, "internal_error",
	concern.JudgementRuleDuplicateOfExcludesJudgement, concern.JudgementRuleDuplicateOfNotFound,
	concern.JudgementRuleDuplicateOfNotEarlier, concern.JudgementRuleTradeoffCostTooLong,
	CategoryConcernLabelled, CategoryConcernLabelFailed,
}

// TestOpenAPI_LabelConcernDocumented pins docs/api/v0.openapi.yaml to the
// handler: the route and operation exist and name every code, rule and audit
// category the handler answers or writes, and the request schema declares
// every field labelConcernRequest decodes. A code added to label_concern.go
// without its docs fails here; update docs/api/v0.openapi.yaml, docs/api/v0.md
// and regenerate site/src/content/docs/reference/api.md
// (scripts/gen-site-reference).
func TestOpenAPI_LabelConcernDocumented(t *testing.T) {
	doc, _ := escapeOpenAPIDoc(t)
	op := escapeMapAt(t, doc, "paths", "/v0/concerns/{concern_id}/label", "post")
	if op["operationId"] != "labelConcern" {
		t.Errorf("operationId = %v, want labelConcern", op["operationId"])
	}
	opYAML, err := yaml.Marshal(op)
	if err != nil {
		t.Fatalf("marshal operation: %v", err)
	}
	for _, code := range labelDocumentedCodes {
		if !strings.Contains(string(opYAML), code) {
			t.Errorf("docs/api/v0.openapi.yaml POST /v0/concerns/{concern_id}/label does not name %q", code)
		}
	}
	props := escapeMapAt(t, doc, "components", "schemas", "LabelConcernRequest", "properties")
	for _, field := range []string{"judgement", "tradeoff_cost", "duplicate_of", "backfill"} {
		if _, ok := props[field]; !ok {
			t.Errorf("docs/api/v0.openapi.yaml LabelConcernRequest is missing %q (labelConcernRequest)", field)
		}
	}
}

// TestLabelConcern_ThroughRouter pins the route registration in handlers.go:
// an anonymous POST through the real router reaches handleLabelConcern's 401,
// where an unregistered pattern would answer the mux's 404.
func TestLabelConcern_ThroughRouter(t *testing.T) {
	s, _, au, cr, runID := labelServer(t)
	row := seedLabelConcern(t, cr, runID, 1)
	req := httptest.NewRequest(http.MethodPost, "/v0/concerns/"+row.ID.String()+"/label", strings.NewReader(`{"judgement":"defect"}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized || decodeLabelErr(t, w).Error.Code != "authentication_required" {
		t.Fatalf("status = %d, want the handler's 401 authentication_required:\n%s", w.Code, w.Body.String())
	}
	if row.Judgement != "" || len(labelPayloads(t, au, CategoryConcernLabelled)) != 0 {
		t.Errorf("an anonymous request wrote: judgement %q", row.Judgement)
	}
}
