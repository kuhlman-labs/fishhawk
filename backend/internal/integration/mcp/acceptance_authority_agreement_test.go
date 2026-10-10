package mcpe2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// authorityOnlyGitHub is a non-nil orchestrator.GitHubAPI so the server's
// SliceIntegrationUnavailable reports slice-integration authority PRESENT. It
// mirrors the helper of the same name in package mcpserver
// (TestAcceptanceIntegrationGate_CrossLayer_PartialThenFull): its methods are
// never called on the paths this test drives, and the embedded nil interface
// would panic if one were.
type authorityOnlyGitHub struct{ orchestrator.GitHubAPI }

// authoritySnapshot is the seeded #4165 snapshot: a top-level decomposed
// parent whose plan and implement stages SUCCEEDED, whose acceptance stage is
// parked at awaiting_host_dispatch, with two succeeded children and a
// plan_decomposed naming them — and NO slices_integrated record.
type authoritySnapshot struct {
	parent   *runpkg.Run
	accStage *runpkg.Stage
	children []uuid.UUID
}

// seedAuthoritySnapshot seeds the snapshot through the run repository.
// Approval condition C3: every stage walks to its state via TransitionStage
// (acceptance pending → awaiting_host_dispatch; plan and implement to
// succeeded), never parkAtGate.
func seedAuthoritySnapshot(t *testing.T, ctx context.Context, repo runpkg.Repository, auditRepo audit.Repository) authoritySnapshot {
	t.Helper()
	inst := int64(4165)
	parent, err := repo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: runpkg.TriggerCLI, RunnerKind: runpkg.RunnerKindLocal, InstallationID: &inst,
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := repo.TransitionRun(ctx, parent.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("parent → running: %v", err)
	}
	newStage := func(runID uuid.UUID, seq int, typ runpkg.StageType, path ...runpkg.StageState) *runpkg.Stage {
		t.Helper()
		st, err := repo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID: runID, Sequence: seq, Type: typ,
			ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("create %s stage: %v", typ, err)
		}
		for _, to := range path {
			if st, err = repo.TransitionStage(ctx, st.ID, to, nil); err != nil {
				t.Fatalf("transition %s → %s: %v", typ, to, err)
			}
		}
		return st
	}
	newStage(parent.ID, 1, runpkg.StageTypePlan,
		runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateSucceeded)
	newStage(parent.ID, 2, runpkg.StageTypeImplement,
		runpkg.StageStateAwaitingChildren, runpkg.StageStateSucceeded)
	acc := newStage(parent.ID, 3, runpkg.StageTypeAcceptance, runpkg.StageStateAwaitingHostDispatch)

	var children []uuid.UUID
	for i := 0; i < 2; i++ {
		idx := i
		c, err := repo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
			TriggerSource: runpkg.TriggerCLI, RunnerKind: runpkg.RunnerKindLocal,
			DecomposedFrom: &parent.ID, ParentRunID: &parent.ID, SliceIndex: &idx, InstallationID: &inst,
		})
		if err != nil {
			t.Fatalf("create child %d: %v", i, err)
		}
		newStage(c.ID, 1, runpkg.StageTypeImplement,
			runpkg.StageStateDispatched, runpkg.StageStateRunning, runpkg.StageStateSucceeded)
		for _, to := range []runpkg.State{runpkg.StateRunning, runpkg.StateSucceeded} {
			if _, err := repo.TransitionRun(ctx, c.ID, to); err != nil {
				t.Fatalf("child %d → %s: %v", i, to, err)
			}
		}
		children = append(children, c.ID)
	}
	appendParentAudit(t, ctx, auditRepo, parent.ID, "plan_decomposed", map[string]any{
		"child_run_ids":          []string{children[0].String(), children[1].String()},
		"effective_max_parallel": 0,
	})
	return authoritySnapshot{parent: parent, accStage: acc, children: children}
}

// appendParentAudit appends one chained audit entry on the run.
func appendParentAudit(t *testing.T, ctx context.Context, repo audit.Repository, runID uuid.UUID, category string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s: %v", category, err)
	}
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	}); err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
}

// postOperator POSTs an empty JSON body with the operator bearer and returns
// the status and the error envelope's code ("" on a 2xx or an undecodable body).
func postOperator(t *testing.T, ctx context.Context, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return resp.StatusCode, e.Error.Code
}

// authorityRunStatus is the slice of fishhawk_get_run_status this test reads.
type authorityRunStatus struct {
	Run struct {
		Capabilities *struct {
			SliceIntegration *struct {
				Available bool   `json:"available"`
				Reason    string `json:"reason"`
			} `json:"slice_integration"`
		} `json:"capabilities"`
	} `json:"run"`
	NextActions *nextActionsView `json:"next_actions"`
}

func getAuthorityRunStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) authorityRunStatus {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status tool returned error: %s", toolContentString(t, result))
	}
	var out authorityRunStatus
	decodeStructured(t, result, &out)
	return out
}

// authorityAwaitOut is the slice of fishhawk_await_children this test reads.
type authorityAwaitOut struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	NextStep *struct {
		Action string            `json:"action"`
		Params map[string]string `json:"params"`
	} `json:"next_step"`
}

func awaitChildrenE2E(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) authorityAwaitOut {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_await_children",
		Arguments: map[string]any{"run_id": runID.String(), "timeout_seconds": 5},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_await_children: %v", err)
	}
	if result.IsError {
		t.Fatalf("await_children tool returned error: %s", toolContentString(t, result))
	}
	var out authorityAwaitOut
	decodeStructured(t, result, &out)
	return out
}

func offersAcceptanceDispatchView(na *nextActionsView) bool {
	if na == nil {
		return false
	}
	for _, a := range na.Actions {
		if (a.Action == "fishhawk_dispatch_stage" || a.Action == "fishhawk_run_stage") && a.Params["stage"] == "acceptance" {
			return true
		}
	}
	return false
}

// TestAcceptanceAuthority_ServerAndMCPSurfacesAgree is the #4165 cross-boundary
// agreement test. It drives the REAL server gate (acceptance-admission AND the
// host-dispatch marker, which share guardDecomposedParentAcceptance) and the
// REAL fishhawk-mcp binary (fishhawk_get_run_status + fishhawk_await_children)
// over one Postgres pool, on the snapshot the old C3 inference misread: NO
// slices_integrated record + a SUCCEEDED parent implement stage.
//
//   - ARM A (authority present: a GitHub client + an installation id): the
//     server refuses 409 acceptance_integration_incomplete, so BOTH MCP
//     surfaces must hold and name the integrate-wave recovery. Appending the
//     record integrate-wave would write then unwedges all three.
//   - ARM B (no authority: Orchestrator nil, the identical snapshot): the
//     server admits, so BOTH MCP surfaces must release.
//   - ARM C (#4221, authority present): the same snapshot plus a surviving
//     wave-0 slices_integrated covering only the first child — a multi-wave
//     fan-out whose FINAL record was lost. The server's wavecoverage gate
//     refuses 409 acceptance_integration_incomplete, so BOTH MCP surfaces must
//     hold and name integrate-wave (never a re-drive of the already-succeeded
//     second child); appending the full record unwedges all three.
func TestAcceptanceAuthority_ServerAndMCPSurfacesAgree(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	auditRepo := audit.NewPostgresRepository(fx.pool)

	t.Run("A: authority present + no record + succeeded parent holds everywhere", func(t *testing.T) {
		snap := seedAuthoritySnapshot(t, ctx, fx.runRepo, auditRepo)
		srv := server.New(server.Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      fx.runRepo,
			AuditRepo:    auditRepo,
			APITokenRepo: fx.apitokenRepo,
			Orchestrator: &orchestrator.Orchestrator{Runs: fx.runRepo, Audit: auditRepo, GitHub: authorityOnlyGitHub{}},
		})
		url := mountServer(t, srv)
		session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, url)
		admissionURL := url + "/v0/stages/" + snap.accStage.ID.String() + "/acceptance-admission"
		hostDispatchURL := url + "/v0/runs/" + snap.parent.ID.String() + "/stages/" + snap.accStage.ID.String() + "/host-dispatch"

		// (1) The server gate refuses on BOTH surfaces that share the guard
		// (approval condition C4: the host-dispatch marker too).
		if status, code := postOperator(t, ctx, admissionURL, fx.operatorTok); status != http.StatusConflict || code != "acceptance_integration_incomplete" {
			t.Fatalf("acceptance-admission = %d %q, want 409 acceptance_integration_incomplete", status, code)
		}
		if status, code := postOperator(t, ctx, hostDispatchURL, fx.operatorTok); status != http.StatusConflict || code != "acceptance_integration_incomplete" {
			t.Fatalf("host-dispatch marker = %d %q, want 409 acceptance_integration_incomplete", status, code)
		}

		// (2) fishhawk_get_run_status reads the server's predicate and HOLDS.
		st := getAuthorityRunStatus(t, ctx, session, snap.parent.ID)
		if st.Run.Capabilities == nil || st.Run.Capabilities.SliceIntegration == nil || !st.Run.Capabilities.SliceIntegration.Available {
			t.Fatalf("run.capabilities.slice_integration = %+v, want available:true through the real wire", st.Run.Capabilities)
		}
		if st.NextActions == nil || st.NextActions.State != "acceptance_held_integration_incomplete" {
			t.Fatalf("next_actions = %+v, want acceptance_held_integration_incomplete (the server refuses 409)", st.NextActions)
		}
		if offersAcceptanceDispatchView(st.NextActions) {
			t.Errorf("next_actions still offers the acceptance dispatch the server refuses: %+v", st.NextActions.Actions)
		}
		if first := st.NextActions.Actions[0]; first.Action != "fishhawk_await_children" || !strings.Contains(first.Reason, "integrate-wave") {
			t.Errorf("first action = %+v, want fishhawk_await_children naming integrate-wave", first)
		}

		// (3) fishhawk_await_children HOLDS too (integration_pending, never
		// children_settled).
		aw := awaitChildrenE2E(t, ctx, session, snap.parent.ID)
		if aw.Status != "integration_pending" {
			t.Fatalf("await_children status = %q, want integration_pending (message %q)", aw.Status, aw.Message)
		}
		if !strings.Contains(aw.Message, "/v0/runs/"+snap.parent.ID.String()+"/integrate-wave") {
			t.Errorf("await message %q must name the integrate-wave recovery", aw.Message)
		}

		// (4) The record integrate-wave would write unwedges all three.
		appendParentAudit(t, ctx, auditRepo, snap.parent.ID, "slices_integrated", map[string]any{
			"consolidated_branch": "fishhawk/run-" + snap.parent.ID.String(),
			"child_run_ids":       []string{snap.children[0].String(), snap.children[1].String()},
		})
		st2 := getAuthorityRunStatus(t, ctx, session, snap.parent.ID)
		if st2.NextActions == nil || st2.NextActions.State == "acceptance_held_integration_incomplete" || !offersAcceptanceDispatchView(st2.NextActions) {
			t.Fatalf("next_actions after the record = %+v, want the acceptance dispatch offered again", st2.NextActions)
		}
		if aw2 := awaitChildrenE2E(t, ctx, session, snap.parent.ID); aw2.Status != "children_settled" {
			t.Errorf("await_children after the record = %q, want children_settled", aw2.Status)
		}
		if status, code := postOperator(t, ctx, admissionURL, fx.operatorTok); code == "acceptance_integration_incomplete" {
			t.Errorf("acceptance-admission after the record = %d %q, want the gate to admit", status, code)
		}
		if status, code := postOperator(t, ctx, hostDispatchURL, fx.operatorTok); code == "acceptance_integration_incomplete" {
			t.Errorf("host-dispatch marker after the record = %d %q, want the gate to admit", status, code)
		}
	})

	t.Run("C: authority present + wave-0 record only + succeeded parent holds everywhere", func(t *testing.T) {
		snap := seedAuthoritySnapshot(t, ctx, fx.runRepo, auditRepo)
		// The surviving between-wave record: it covers only the first child.
		appendParentAudit(t, ctx, auditRepo, snap.parent.ID, "slices_integrated", map[string]any{
			"consolidated_branch": "fishhawk/run-" + snap.parent.ID.String(),
			"child_run_ids":       []string{snap.children[0].String()},
		})
		srv := server.New(server.Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      fx.runRepo,
			AuditRepo:    auditRepo,
			APITokenRepo: fx.apitokenRepo,
			Orchestrator: &orchestrator.Orchestrator{Runs: fx.runRepo, Audit: auditRepo, GitHub: authorityOnlyGitHub{}},
		})
		url := mountServer(t, srv)
		session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, url)
		admissionURL := url + "/v0/stages/" + snap.accStage.ID.String() + "/acceptance-admission"
		hostDispatchURL := url + "/v0/runs/" + snap.parent.ID.String() + "/stages/" + snap.accStage.ID.String() + "/host-dispatch"
		integrateWave := "/v0/runs/" + snap.parent.ID.String() + "/integrate-wave"
		uncovered := snap.children[1].String()

		// (1) The server gate refuses the partial snapshot on both surfaces.
		if status, code := postOperator(t, ctx, admissionURL, fx.operatorTok); status != http.StatusConflict || code != "acceptance_integration_incomplete" {
			t.Fatalf("acceptance-admission = %d %q, want 409 acceptance_integration_incomplete", status, code)
		}
		if status, code := postOperator(t, ctx, hostDispatchURL, fx.operatorTok); status != http.StatusConflict || code != "acceptance_integration_incomplete" {
			t.Fatalf("host-dispatch marker = %d %q, want 409 acceptance_integration_incomplete", status, code)
		}

		// (2) fishhawk_get_run_status HOLDS and names integrate-wave.
		st := getAuthorityRunStatus(t, ctx, session, snap.parent.ID)
		if st.NextActions == nil || st.NextActions.State != "acceptance_held_integration_incomplete" {
			t.Fatalf("next_actions = %+v, want acceptance_held_integration_incomplete (the server refuses 409)", st.NextActions)
		}
		if offersAcceptanceDispatchView(st.NextActions) {
			t.Errorf("next_actions still offers the acceptance dispatch the server refuses: %+v", st.NextActions.Actions)
		}
		first := st.NextActions.Actions[0]
		if first.Action != "fishhawk_await_children" || !strings.Contains(first.Reason, integrateWave) || !strings.Contains(first.Reason, uncovered) {
			t.Errorf("first action = %+v, want fishhawk_await_children naming %s and the uncovered child %s", first, integrateWave, uncovered)
		}
		if strings.Contains(first.Reason, "re-drive") {
			t.Errorf("hold reason %q must not advise a re-drive: the uncovered child already succeeded", first.Reason)
		}

		// (3) fishhawk_await_children HOLDS on the PARENT and names
		// integrate-wave, not a re-drive of the uncovered child.
		aw := awaitChildrenE2E(t, ctx, session, snap.parent.ID)
		if aw.Status != "integration_pending" {
			t.Fatalf("await_children status = %q, want integration_pending (message %q)", aw.Status, aw.Message)
		}
		if !strings.Contains(aw.Message, integrateWave) {
			t.Errorf("await message %q must name the integrate-wave recovery", aw.Message)
		}
		if strings.Contains(aw.Message, "re-drive") {
			t.Errorf("await message %q must not advise a re-drive: the uncovered child already succeeded", aw.Message)
		}
		if aw.NextStep == nil || aw.NextStep.Action != "fishhawk_get_run_status" || aw.NextStep.Params["run_id"] != snap.parent.ID.String() {
			t.Errorf("await next_step = %+v, want fishhawk_get_run_status on the parent (not the uncovered child)", aw.NextStep)
		}

		// (4) The full record integrate-wave would write unwedges all three.
		appendParentAudit(t, ctx, auditRepo, snap.parent.ID, "slices_integrated", map[string]any{
			"consolidated_branch": "fishhawk/run-" + snap.parent.ID.String(),
			"child_run_ids":       []string{snap.children[0].String(), snap.children[1].String()},
		})
		st2 := getAuthorityRunStatus(t, ctx, session, snap.parent.ID)
		if st2.NextActions == nil || st2.NextActions.State == "acceptance_held_integration_incomplete" || !offersAcceptanceDispatchView(st2.NextActions) {
			t.Fatalf("next_actions after the full record = %+v, want the acceptance dispatch offered again", st2.NextActions)
		}
		if aw2 := awaitChildrenE2E(t, ctx, session, snap.parent.ID); aw2.Status != "children_settled" {
			t.Errorf("await_children after the full record = %q, want children_settled", aw2.Status)
		}
		if status, code := postOperator(t, ctx, admissionURL, fx.operatorTok); code == "acceptance_integration_incomplete" {
			t.Errorf("acceptance-admission after the full record = %d %q, want the gate to admit", status, code)
		}
		if status, code := postOperator(t, ctx, hostDispatchURL, fx.operatorTok); code == "acceptance_integration_incomplete" {
			t.Errorf("host-dispatch marker after the full record = %d %q, want the gate to admit", status, code)
		}
	})

	t.Run("B: no authority + no record + succeeded parent releases everywhere", func(t *testing.T) {
		snap := seedAuthoritySnapshot(t, ctx, fx.runRepo, auditRepo)
		srv := server.New(server.Config{
			Addr:         "127.0.0.1:0",
			RunRepo:      fx.runRepo,
			AuditRepo:    auditRepo,
			APITokenRepo: fx.apitokenRepo,
		})
		url := mountServer(t, srv)
		session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, url)

		st := getAuthorityRunStatus(t, ctx, session, snap.parent.ID)
		if st.Run.Capabilities == nil || st.Run.Capabilities.SliceIntegration == nil || st.Run.Capabilities.SliceIntegration.Available {
			t.Fatalf("run.capabilities.slice_integration = %+v, want available:false", st.Run.Capabilities)
		}
		if st.NextActions == nil || st.NextActions.State != "acceptance_pending" || !offersAcceptanceDispatchView(st.NextActions) {
			t.Fatalf("next_actions = %+v, want acceptance_pending with the dispatch (the server stands down)", st.NextActions)
		}
		aw := awaitChildrenE2E(t, ctx, session, snap.parent.ID)
		if aw.Status != "children_settled" {
			t.Fatalf("await_children status = %q, want children_settled (message %q)", aw.Status, aw.Message)
		}
		if aw.NextStep == nil || aw.NextStep.Action != "fishhawk_get_run_status" || aw.NextStep.Params["run_id"] != snap.parent.ID.String() {
			t.Errorf("await next_step = %+v, want fishhawk_get_run_status on the advanced parent", aw.NextStep)
		}
		admissionURL := url + "/v0/stages/" + snap.accStage.ID.String() + "/acceptance-admission"
		if status, code := postOperator(t, ctx, admissionURL, fx.operatorTok); status == http.StatusConflict || code == "acceptance_integration_incomplete" {
			t.Errorf("acceptance-admission = %d %q, want the gate to stand down (no authority)", status, code)
		}
	})
}
