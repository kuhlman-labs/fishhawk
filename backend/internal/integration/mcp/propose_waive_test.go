package mcpe2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// pwGateConcern is the subset of a fishhawk_get_gate_view open concern this
// test reads: its state and the pending waive_proposal block.
type pwGateConcern struct {
	ID            uuid.UUID `json:"id"`
	State         string    `json:"state"`
	WaiveProposal *struct {
		ProposalSequence    int64  `json:"proposal_sequence"`
		ProposedBy          string `json:"proposed_by"`
		ProposedByActorKind string `json:"proposed_by_actor_kind"`
		HeadSHA             string `json:"head_sha"`
		CheckKey            string `json:"check_key"`
		Reason              string `json:"reason"`
		Evidence            string `json:"evidence"`
		AgentAuthored       bool   `json:"agent_authored"`
	} `json:"waive_proposal"`
}

// TestProposeWaive_E2E is the E80.28 / #5229 (ADR-143 A1) cross-boundary
// seam: fishhawk_propose_waive's input -> the REAL fishhawk-mcp binary's
// client body -> the REST handler's run-bound, credential and head guards ->
// the production postgresRepo's AppendChainedGuarded under the real run-row
// lock -> the fishhawk_get_gate_view waive_proposal block, the
// fishhawk_get_run_status waive_proposals mirror and its next_actions fold.
// It then pins the channel rule end to end: tools/list carries NO confirm or
// reject tool, and a REST confirm on the harness's static bearer is refused
// with 403 concern_requires_session, the concern still raised and the
// proposal still pending.
func TestProposeWaive_E2E(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	concernRepo := concern.NewPostgresRepository(fx.pool)
	auditRepo := audit.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
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
		WorkflowSHA:     "sha-5229-e2e",
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

	// Seed BY CONSTRUCTION: the recorded run head A, then one raised
	// server_check concern with its server-written check_key.
	const headA = "a5229a5229a5229a5229a5229a5229a5229a5229"
	const checkKey = "diff_secrets:testdata/fixture.pem:private_key"
	kind := audit.ActorSystem
	raw, _ := json.Marshal(map[string]any{"head_sha": headA, "pr_number": 7})
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: rn.ID, StageID: &stage.ID, Timestamp: time.Now().UTC(), Category: "pull_request_opened", ActorKind: &kind, Payload: raw,
	}); err != nil {
		t.Fatalf("AppendChained pull_request_opened: %v", err)
	}
	rows, err := concernRepo.InsertRaised(ctx, concern.InsertRaisedParams{
		RunID: rn.ID, StageID: stage.ID, StageKind: concern.StageKindImplement,
		Provenance: concern.ProvenanceServerCheck, OriginReviewSequence: 1,
		Concerns: []concern.RaisedConcern{{
			Severity: "high", Category: "diff_secrets_detected", CheckKey: checkKey,
			Note: "a private-key-shaped string was added at testdata/fixture.pem",
		}},
	})
	if err != nil {
		t.Fatalf("InsertRaised: %v", err)
	}
	concernID := rows[0].ID

	// The proposer is an operator-agent credential (not run-bound), so the
	// recorded actor kind is agent.
	agentSubject := operatorrole.TokenSubjectPrefix + "operator-role-v0"
	agentTok, err := fx.apitokenRepo.Issue(ctx, agentSubject, []string{"read:runs", "read:audit", "write:stages"})
	if err != nil {
		t.Fatalf("Issue operator-agent apitoken: %v", err)
	}
	session := connectMCPClient(t, ctx, fx.mcpBinary, agentTok.PlainText, httpSrv.URL)

	// 1. tools/list carries the propose tool and NO confirm or reject tool.
	list, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	var sawPropose bool
	for _, tool := range list.Tools {
		if tool.Name == "fishhawk_propose_waive" {
			sawPropose = true
		}
		if strings.Contains(tool.Name, "confirm_waive") || strings.Contains(tool.Name, "reject_waive") || strings.Contains(tool.Name, "waive_proposal") {
			t.Errorf("tools/list carries %q; confirm and reject are Web UI session-only (ADR-143 A1)", tool.Name)
		}
	}
	if !sawPropose {
		t.Fatal("tools/list does not carry fishhawk_propose_waive")
	}

	// 2. Propose through the real binary.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_propose_waive",
		Arguments: map[string]any{
			"concern_id": concernID.String(),
			"reason":     "the flagged file is a generated test fixture, not a live credential",
			"evidence":   "testdata/fixture.pem is written by the fixture generator and used only by its unit test",
			"judgement":  "no_defect",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_propose_waive: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_propose_waive returned a tool error: %s", toolContentString(t, res))
	}
	var proposed struct {
		Proposal struct {
			ProposalSequence int64  `json:"proposal_sequence"`
			HeadSHA          string `json:"head_sha"`
			CheckKey         string `json:"check_key"`
			AgentAuthored    bool   `json:"agent_authored"`
		} `json:"proposal"`
		ProposedBy string `json:"proposed_by"`
		NextStep   string `json:"next_step"`
	}
	decodeStructured(t, res, &proposed)
	seq := proposed.Proposal.ProposalSequence
	if seq <= 0 || proposed.Proposal.HeadSHA != headA || proposed.Proposal.CheckKey != checkKey || !proposed.Proposal.AgentAuthored ||
		proposed.ProposedBy != agentSubject || !strings.Contains(proposed.NextStep, "Fishhawk Web UI") {
		t.Fatalf("propose output = %+v, want a proposal at head A with the server-written check_key, proposed_by %s", proposed, agentSubject)
	}

	// Committed state, read back from Postgres: exactly one proposed row by
	// the agent, no concern_waived row, the concern row still raised.
	proposedRows, err := auditRepo.ListForRunByCategory(ctx, rn.ID, "concern_waive_proposed")
	if err != nil {
		t.Fatalf("ListForRunByCategory concern_waive_proposed: %v", err)
	}
	if len(proposedRows) != 1 || proposedRows[0].Sequence != seq || proposedRows[0].ActorSubject == nil || *proposedRows[0].ActorSubject != agentSubject ||
		proposedRows[0].ActorKind == nil || *proposedRows[0].ActorKind != audit.ActorAgent {
		t.Fatalf("concern_waive_proposed rows = %+v, want exactly one at sequence %d by the agent", proposedRows, seq)
	}
	assertNoWaived := func(when string) {
		t.Helper()
		waived, err := auditRepo.ListForRunByCategory(ctx, rn.ID, "concern_waived")
		if err != nil {
			t.Fatalf("ListForRunByCategory concern_waived: %v", err)
		}
		if len(waived) != 0 {
			t.Errorf("%s: concern_waived rows = %d, want 0 (a proposal clears nothing)", when, len(waived))
		}
		got, err := concernRepo.GetByIDs(ctx, []uuid.UUID{concernID})
		if err != nil || len(got) != 1 {
			t.Fatalf("concern GetByIDs = %v, %v; want the one row", got, err)
		}
		if got[0].State != concern.StateRaised {
			t.Errorf("%s: concern state = %q, want raised", when, got[0].State)
		}
	}
	assertNoWaived("after propose")

	// 3. fishhawk_get_gate_view: the concern is open, raised, and carries the
	// pending waive_proposal at head A.
	gvRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_get_gate_view", Arguments: map[string]any{"run_id": rn.ID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_gate_view: %v", err)
	}
	if gvRes.IsError {
		t.Fatalf("fishhawk_get_gate_view returned a tool error: %s", toolContentString(t, gvRes))
	}
	var gv struct {
		GateView struct {
			Open []pwGateConcern `json:"open"`
		} `json:"gate_view"`
	}
	decodeStructured(t, gvRes, &gv)
	var open *pwGateConcern
	for i := range gv.GateView.Open {
		if gv.GateView.Open[i].ID == concernID {
			open = &gv.GateView.Open[i]
		}
	}
	if open == nil {
		t.Fatalf("concern %s missing from the gate view's open list", concernID)
	}
	if open.State != "raised" || open.WaiveProposal == nil || open.WaiveProposal.ProposalSequence != seq ||
		open.WaiveProposal.HeadSHA != headA || open.WaiveProposal.CheckKey != checkKey || !open.WaiveProposal.AgentAuthored ||
		open.WaiveProposal.ProposedBy != agentSubject || open.WaiveProposal.ProposedByActorKind != "agent" {
		t.Fatalf("gate-view concern = %+v (proposal %+v), want raised carrying the pending proposal at head A", open, open.WaiveProposal)
	}

	// 4. fishhawk_get_run_status: next_actions carries one display-only
	// confirm_waive_proposal entry naming the proposal.
	rsRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_get_run_status", Arguments: map[string]any{"run_id": rn.ID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if rsRes.IsError {
		t.Fatalf("fishhawk_get_run_status returned a tool error: %s", toolContentString(t, rsRes))
	}
	var rs struct {
		NextActions *nextActionsView `json:"next_actions"`
	}
	decodeStructured(t, rsRes, &rs)
	if rs.NextActions == nil {
		t.Fatal("next_actions absent")
	}
	var confirms int
	for _, a := range rs.NextActions.Actions {
		if a.Action != "confirm_waive_proposal" {
			continue
		}
		confirms++
		if a.Consumes != "none" || a.Params["concern_id"] != concernID.String() || a.Params["run_id"] != rn.ID.String() ||
			a.Params["stage_id"] != stage.ID.String() || a.Params["proposal_sequence"] != strconv.FormatInt(seq, 10) ||
			!strings.Contains(a.Reason, "Fishhawk Web UI") || !strings.Contains(a.Reason, "There is no MCP or CLI confirm") {
			t.Errorf("confirm_waive_proposal = %+v, want consumes none, the four params and the Web UI reason", a)
		}
	}
	if confirms != 1 {
		t.Errorf("confirm_waive_proposal entries = %d, want 1 (actions %+v)", confirms, rs.NextActions.Actions)
	}

	// 5. A REST confirm on the harness's STATIC bearer (a human-subject
	// apitoken, not a Web UI session) is refused with concern_requires_session.
	body, _ := json.Marshal(map[string]any{"proposal_sequence": seq})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		httpSrv.URL+"/v0/concerns/"+concernID.String()+"/waive-proposal/confirm", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new confirm request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+fx.operatorTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST confirm: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(respBody, &refusal)
	if resp.StatusCode != http.StatusForbidden || refusal.Error.Code != "concern_requires_session" {
		t.Fatalf("static-bearer confirm = %d %s, want 403 concern_requires_session", resp.StatusCode, respBody)
	}
	assertNoWaived("after the refused confirm")
	stillProposed, err := auditRepo.ListForRunByCategory(ctx, rn.ID, "concern_waive_proposed")
	if err != nil {
		t.Fatalf("ListForRunByCategory concern_waive_proposed: %v", err)
	}
	if len(stillProposed) != 1 {
		t.Errorf("concern_waive_proposed rows after the refused confirm = %d, want 1", len(stillProposed))
	}
}
