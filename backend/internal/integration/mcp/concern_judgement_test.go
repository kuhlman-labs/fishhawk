package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// cjGateConcern is the subset of a fishhawk_get_gate_view open or settled
// concern this test reads: its state and the judgement record.
type cjGateConcern struct {
	ID                 string `json:"id"`
	State              string `json:"state"`
	Outcome            string `json:"outcome"`
	Judgement          string `json:"judgement"`
	JudgementActorKind string `json:"judgement_actor_kind"`
	JudgementBulk      bool   `json:"judgement_bulk"`
	TradeoffCost       string `json:"tradeoff_cost"`
}

// cjGateView reads every concern of runID through the real fishhawk-mcp
// binary's fishhawk_get_gate_view, open and settled merged by id.
func cjGateView(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) map[string]cjGateConcern {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_gate_view",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_gate_view: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_get_gate_view returned a tool error: %s", toolContentString(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal gate view: %v", err)
	}
	var out struct {
		GateView struct {
			Open    []cjGateConcern `json:"open"`
			Settled []cjGateConcern `json:"settled"`
		} `json:"gate_view"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode gate view: %v\n%s", err, raw)
	}
	all := map[string]cjGateConcern{}
	for _, c := range append(out.GateView.Open, out.GateView.Settled...) {
		all[c.ID] = c
	}
	return all
}

// TestE2E_ConcernJudgement_RequiredAndRecorded is the E94.7 / #4840 (ADR-117
// D1 phase 2) cross-boundary seam: MCP tool input -> the fishhawk-mcp binary's
// client body -> the REST handler's judgement requirement -> the REAL Postgres
// concern store -> the fishhawk_get_gate_view read surface. The MCP tools
// declare judgement optional and pre-check nothing, so the backend is the ONE
// refusal authority: deleting any verb's REST guard turns its refusal subtest
// RED here (the call would reach the store and fail, or commit). The run is in
// a non-Fishhawk repository (ADR-110).
func TestE2E_ConcernJudgement_RequiredAndRecorded(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	concernRepo := concern.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    audit.NewPostgresRepository(fx.pool),
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		ArtifactRepo: artifact.NewPostgresRepository(fx.pool),
		ConcernRepo:  concernRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	requiresCharter := false
	rn, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:            "acme/widgets",
		WorkflowID:      "feature_change",
		WorkflowSHA:     "sha-4840-e2e",
		TriggerSource:   runpkg.TriggerCLI,
		RequiresCharter: &requiresCharter,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        rn.ID,
		Sequence:     1,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	notes := []string{"refusal target", "routed refusal target", "waive A", "waive B", "waive C", "resolve A", "resolve B"}
	raised := make([]concern.RaisedConcern, len(notes))
	for i, n := range notes {
		raised[i] = concern.RaisedConcern{Severity: "medium", Category: "correctness", Note: n}
	}
	rows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: rn.ID, StageID: stage.ID, StageKind: concern.StageKindImplement,
		ReviewerModel: "gpt-6-astra", OriginReviewSequence: 1, Concerns: raised,
	})
	if err != nil {
		t.Fatalf("InsertRaised: %v", err)
	}
	open1, routed1 := rows[0].ID, rows[1].ID
	waiveA, waiveB, waiveC := rows[2].ID, rows[3].ID, rows[4].ID
	resolveA, resolveB := rows[5].ID, rows[6].ID
	if err := concernRepo.MarkAddressedPending(ctx, []uuid.UUID{routed1, resolveA, resolveB}, "routed by the operator"); err != nil {
		t.Fatalf("MarkAddressedPending: %v", err)
	}

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	runID := rn.ID.String()

	// Refusals. Each names validation_failed and the field (or the rule), and
	// the target concern reads back open and unjudged afterwards.
	refusals := []struct {
		name, tool string
		args       map[string]any
		target     uuid.UUID
		wantInText []string
	}{
		{"waive: missing judgement", "fishhawk_waive_concern",
			map[string]any{"concern_id": open1.String(), "reason": "not blocking"}, open1,
			[]string{"validation_failed", `"field":"judgement"`, "judgement_required"}},
		{"waive: tradeoff without cost", "fishhawk_waive_concern",
			map[string]any{"concern_id": open1.String(), "reason": "accepted", "judgement": "tradeoff"}, open1,
			[]string{"validation_failed", `"field":"tradeoff_cost"`, "tradeoff_cost_required"}},
		{"waive: contradicting reason prefix", "fishhawk_waive_concern",
			map[string]any{"concern_id": open1.String(), "reason": "false_positive: the guard is upstream", "judgement": "defect"}, open1,
			[]string{"validation_failed", "judgement_conflicts_reason_prefix", "false_positive:"}},
		{"bulk waive: missing judgement", "fishhawk_waive_concerns",
			map[string]any{"run_id": runID, "concern_ids": []string{open1.String()}, "reason": "not blocking"}, open1,
			[]string{"validation_failed", `"field":"judgement"`, "judgement_required"}},
		{"bulk waive: tradeoff without cost", "fishhawk_waive_concerns",
			map[string]any{"run_id": runID, "concern_ids": []string{open1.String()}, "reason": "accepted", "judgement": "tradeoff"}, open1,
			[]string{"validation_failed", `"field":"tradeoff_cost"`, "tradeoff_cost_required"}},
		{"resolve: missing judgement", "fishhawk_resolve_concerns",
			map[string]any{"run_id": runID, "concern_ids": []string{routed1.String()}, "evidence": "re-ran it"}, routed1,
			[]string{"validation_failed", `"field":"judgement"`, "judgement_required"}},
		{"resolve: tradeoff without cost", "fishhawk_resolve_concerns",
			map[string]any{"run_id": runID, "concern_ids": []string{routed1.String()}, "evidence": "re-ran it", "judgement": "tradeoff"}, routed1,
			[]string{"validation_failed", `"field":"tradeoff_cost"`, "tradeoff_cost_required"}},
		{"defer: missing judgement", "fishhawk_defer_concern",
			map[string]any{"concern_id": open1.String(), "parent_epic": "#1"}, open1,
			[]string{"validation_failed", `"field":"judgement"`, "judgement_required"}},
		{"defer: tradeoff without cost", "fishhawk_defer_concern",
			map[string]any{"concern_id": open1.String(), "parent_epic": "#1", "judgement": "tradeoff"}, open1,
			[]string{"validation_failed", `"field":"tradeoff_cost"`, "tradeoff_cost_required"}},
	}
	for _, rc := range refusals {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: rc.tool, Arguments: rc.args})
		if err != nil {
			t.Fatalf("%s: CallTool %s: %v", rc.name, rc.tool, err)
		}
		text := toolContentString(t, res)
		if !res.IsError {
			t.Errorf("%s: %s succeeded; want a tool error:\n%s", rc.name, rc.tool, text)
		}
		for _, want := range rc.wantInText {
			if !strings.Contains(text, want) {
				t.Errorf("%s: tool error does not name %q:\n%s", rc.name, want, text)
			}
		}
	}
	gv := cjGateView(t, ctx, session, rn.ID)
	for id, wantState := range map[uuid.UUID]concern.State{open1: concern.StateRaised, routed1: concern.StateAddressedPending} {
		c, ok := gv[id.String()]
		if !ok || c.State != string(wantState) || c.Judgement != string(concern.JudgementUnlabelled) || c.Outcome != "" || c.JudgementActorKind != "" {
			t.Errorf("concern %s after the refusals = %+v (present %v), want %s and unlabelled with no outcome", id, c, ok, wantState)
		}
	}

	// Recording: a 2-concern tradeoff bulk waive, a 1-concern bulk waive and a
	// 2-concern defect resolve, read back through the gate view.
	calls := []struct {
		tool string
		args map[string]any
	}{
		{"fishhawk_waive_concerns", map[string]any{
			"run_id": runID, "concern_ids": []string{waiveA.String(), waiveB.String()},
			"reason": "both accepted for this merge", "judgement": "tradeoff", "tradeoff_cost": "two known gaps ship",
		}},
		{"fishhawk_waive_concerns", map[string]any{
			"run_id": runID, "concern_ids": []string{waiveC.String()}, "reason": "naming only", "judgement": "preference",
		}},
		{"fishhawk_resolve_concerns", map[string]any{
			"run_id": runID, "concern_ids": []string{resolveA.String(), resolveB.String()},
			"evidence": "re-ran both reproductions; they pass", "judgement": "defect",
		}},
	}
	for _, c := range calls {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
		if err != nil {
			t.Fatalf("CallTool %s: %v", c.tool, err)
		}
		if res.IsError {
			t.Fatalf("%s returned a tool error: %s", c.tool, toolContentString(t, res))
		}
	}
	gv = cjGateView(t, ctx, session, rn.ID)
	want := map[uuid.UUID]cjGateConcern{
		waiveA:   {State: "waived", Outcome: "waived", Judgement: "tradeoff", JudgementActorKind: "user", JudgementBulk: true, TradeoffCost: "two known gaps ship"},
		waiveB:   {State: "waived", Outcome: "waived", Judgement: "tradeoff", JudgementActorKind: "user", JudgementBulk: true, TradeoffCost: "two known gaps ship"},
		waiveC:   {State: "waived", Outcome: "waived", Judgement: "preference", JudgementActorKind: "user", JudgementBulk: false},
		resolveA: {State: "addressed", Outcome: "fixed", Judgement: "defect", JudgementActorKind: "user", JudgementBulk: true},
		resolveB: {State: "addressed", Outcome: "fixed", Judgement: "defect", JudgementActorKind: "user", JudgementBulk: true},
	}
	for id, w := range want {
		w.ID = id.String()
		if got := gv[id.String()]; got != w {
			t.Errorf("gate view concern %s = %+v, want %+v", id, got, w)
		}
	}
}
