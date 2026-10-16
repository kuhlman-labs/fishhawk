package mcpe2e_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// scopeOverlapWorkflowSpec is a plain plan -> implement feature_change spec.
var scopeOverlapWorkflowSpec = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
`)

// e2eScopeOverlap decodes the scope_overlap block the way an MCP consumer
// would, independently of the mcpserver mirror: a tag drift between the
// server's scopeOverlapPayload and the client mirror drops a field here.
type e2eScopeOverlap struct {
	RunID             string `json:"run_id"`
	SelfScopeResolved bool   `json:"self_scope_resolved"`
	Overlaps          []struct {
		RunID          string   `json:"run_id"`
		IssueRef       string   `json:"issue_ref"`
		DecomposedFrom string   `json:"decomposed_from"`
		Stage          string   `json:"stage"`
		StageState     string   `json:"stage_state"`
		Paths          []string `json:"paths"`
	} `json:"overlaps"`
	ScannedRuns  int  `json:"scanned_runs"`
	UnscopedRuns int  `json:"unscoped_runs"`
	Truncated    bool `json:"truncated"`
}

// overlapPaths maps each reported run id to its shared paths.
func (o *e2eScopeOverlap) overlapPaths() map[string][]string {
	out := map[string][]string{}
	if o == nil {
		return out
	}
	for _, e := range o.Overlaps {
		out[e.RunID] = e.Paths
	}
	return out
}

// overlapRun is one seeded run: its row, plan stage and signing key.
type overlapRun struct {
	row   *runpkg.Run
	stage *runpkg.Stage
	priv  ed25519.PrivateKey
	ref   string
}

// TestE2E_ScopeOverlap_ShipToMCPSurfaces is the E83.60 / #4118 cross-layer
// test: plans shipped through the REAL signed handleShipPlan path into
// Postgres, the server's effective-scope intersection, and the result read
// back through the REAL fishhawk-mcp binary's fishhawk_get_plan and
// fishhawk_get_gate_view, plus the plan_scope_overlap row in the audit repo.
//
// Fixture (repo acme/overlap): A[shared.go,a.go], B[shared.go,b.go],
// C[c.go] + an APPROVED amendment adding a.go + a PENDING one adding b.go,
// D[shared.go] cancelled. A reports B on shared.go and C on a.go (the
// approved-amendment union), never D (terminal); B reports A only (C's pending
// b.go never counts); C reports A on a.go only. B's ship records one
// plan_scope_overlap row naming A. Approval condition 1: a run-bound token for
// A sees no scope_overlap on either surface — none of B's or C's run ids or
// issue refs reach it.
func TestE2E_ScopeOverlap_ShipToMCPSurfaces(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	signingRepo := signing.NewPostgresRepository(fx.pool)
	artifactRepo := artifact.NewPostgresRepository(fx.pool)
	amendRepo := scopeamendment.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            fx.runRepo,
		AuditRepo:          auditRepo,
		SigningRepo:        signingRepo,
		ArtifactRepo:       artifactRepo,
		ApprovalRepo:       approval.NewPostgresRepository(fx.pool),
		ConcernRepo:        concern.NewPostgresRepository(fx.pool),
		ScopeAmendmentRepo: amendRepo,
		MCPTokenRepo:       fx.mcpTokenRepo,
		APITokenRepo:       fx.apitokenRepo,
		GitHub:             githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	newRun := func(n string) *overlapRun {
		t.Helper()
		ref := "acme/overlap#" + n
		row, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: "acme/overlap", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
			TriggerSource: runpkg.TriggerCLI, TriggerRef: &ref, WorkflowSpec: scopeOverlapWorkflowSpec,
		})
		if err != nil {
			t.Fatalf("CreateRun %s: %v", n, err)
		}
		issued, err := signingRepo.Issue(ctx, row.ID, time.Hour)
		if err != nil {
			t.Fatalf("Issue signing key %s: %v", n, err)
		}
		st, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID: row.ID, Sequence: 1, Type: runpkg.StageTypePlan, ExecutorKind: runpkg.ExecutorAgent,
			ExecutorRef: "fishhawk/runner@v1", RequiresApproval: true,
		})
		if err != nil {
			t.Fatalf("CreateStage(plan) %s: %v", n, err)
		}
		walkToRunning(t, ctx, fx, st.ID)
		return &overlapRun{row: row, stage: st, priv: issued.PrivateKey, ref: ref}
	}
	ship := func(r *overlapRun, files ...string) {
		t.Helper()
		resp := shipPlanSigned(t, ctx, httpSrv.URL, r.row.ID, r.stage.ID, r.priv, regressionPlanJSON("plan "+r.ref, files))
		_ = resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Fatalf("ship plan for %s status = %d, want 201", r.ref, resp.StatusCode)
		}
	}

	a, b, c, d := newRun("1"), newRun("2"), newRun("3"), newRun("4")

	// D shares shared.go but is CANCELLED before anything else ships.
	ship(d, "shared.go")
	if _, err := fx.runRepo.TransitionRun(ctx, d.row.ID, runpkg.StateCancelled); err != nil {
		t.Fatalf("cancel D: %v", err)
	}

	// C's amendments hang off an implement stage (the FK).
	cImpl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: c.row.ID, Sequence: 2, Type: runpkg.StageTypeImplement, ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef: "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(implement) C: %v", err)
	}
	approved, err := amendRepo.Create(ctx, scopeamendment.CreateParams{
		RunID: c.row.ID, StageID: cImpl.ID, Reason: "needs a.go",
		Paths: []scopeamendment.PathEntry{{Path: "a.go", Operation: "modify"}},
	})
	if err != nil {
		t.Fatalf("create approved amendment: %v", err)
	}
	if _, err := amendRepo.Decide(ctx, scopeamendment.DecideParams{
		ID: approved.ID, Status: scopeamendment.StatusApproved, DecidedBy: "github:op",
	}); err != nil {
		t.Fatalf("approve amendment: %v", err)
	}
	if _, err := amendRepo.Create(ctx, scopeamendment.CreateParams{
		RunID: c.row.ID, StageID: cImpl.ID, Reason: "wants b.go",
		Paths: []scopeamendment.PathEntry{{Path: "b.go", Operation: "modify"}},
	}); err != nil {
		t.Fatalf("create pending amendment: %v", err)
	}

	ship(a, "shared.go", "a.go")
	ship(b, "shared.go", "b.go")
	ship(c, "c.go")

	// --- fishhawk_get_plan through the REAL binary, operator token ---
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	getPlanOverlap := func(s *mcp.ClientSession, r *overlapRun) (*e2eScopeOverlap, string) {
		t.Helper()
		res, err := s.CallTool(ctx, &mcp.CallToolParams{
			Name: "fishhawk_get_plan", Arguments: map[string]any{"run_id": r.row.ID.String()},
		})
		if err != nil {
			t.Fatalf("CallTool fishhawk_get_plan %s: %v", r.ref, err)
		}
		if res.IsError {
			t.Fatalf("fishhawk_get_plan %s returned error: %s", r.ref, toolContentString(t, res))
		}
		var out struct {
			Status       string           `json:"status"`
			ScopeOverlap *e2eScopeOverlap `json:"scope_overlap"`
		}
		decodeStructured(t, res, &out)
		if out.Status != "available" {
			t.Fatalf("fishhawk_get_plan %s status = %q, want available", r.ref, out.Status)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		return out.ScopeOverlap, string(raw)
	}

	aOv, _ := getPlanOverlap(session, a)
	if aOv == nil || !aOv.SelfScopeResolved || aOv.RunID != a.row.ID.String() {
		t.Fatalf("A scope_overlap = %+v, want a resolved block for A", aOv)
	}
	want := map[string][]string{b.row.ID.String(): {"shared.go"}, c.row.ID.String(): {"a.go"}}
	if got := aOv.overlapPaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("A overlaps = %v, want B[shared.go] and C[a.go] (approved amendment), never cancelled D %s", got, d.row.ID)
	}
	for _, e := range aOv.Overlaps {
		if e.RunID == b.row.ID.String() && (e.IssueRef != b.ref || e.Stage != "plan" || e.StageState == "") {
			t.Errorf("A's entry for B = %+v, want issue_ref %s and B's current plan stage", e, b.ref)
		}
	}

	bOv, _ := getPlanOverlap(session, b)
	if got, want := bOv.overlapPaths(), map[string][]string{a.row.ID.String(): {"shared.go"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("B overlaps = %v, want A[shared.go] only (C's PENDING b.go never counts)", got)
	}
	cOv, _ := getPlanOverlap(session, c)
	if got, want := cOv.overlapPaths(), map[string][]string{a.row.ID.String(): {"a.go"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("C overlaps = %v, want A[a.go] only", got)
	}

	// --- the plan-gate snapshot: B's ship recorded one row naming A ---
	rows, err := auditRepo.ListForRunByCategory(ctx, b.row.ID, "plan_scope_overlap")
	if err != nil {
		t.Fatalf("list plan_scope_overlap: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("B plan_scope_overlap rows = %d, want exactly 1", len(rows))
	}
	if rows[0].StageID == nil || *rows[0].StageID != b.stage.ID {
		t.Errorf("row stage_id = %v, want B's plan stage %s", rows[0].StageID, b.stage.ID)
	}
	var snap e2eScopeOverlap
	if err := json.Unmarshal(rows[0].Payload, &snap); err != nil {
		t.Fatalf("decode plan_scope_overlap payload: %v", err)
	}
	if got, want := snap.overlapPaths(), map[string][]string{a.row.ID.String(): {"shared.go"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("B's recorded overlap = %v, want A[shared.go]", got)
	}

	// --- fishhawk_get_gate_view on A carries the same block ---
	gvRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_get_gate_view", Arguments: map[string]any{"run_id": a.row.ID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_gate_view: %v", err)
	}
	if gvRes.IsError {
		t.Fatalf("fishhawk_get_gate_view returned error: %s", toolContentString(t, gvRes))
	}
	var gv struct {
		GateView struct {
			ScopeOverlap *e2eScopeOverlap `json:"scope_overlap"`
		} `json:"gate_view"`
	}
	decodeStructured(t, gvRes, &gv)
	if !reflect.DeepEqual(gv.GateView.ScopeOverlap, aOv) {
		t.Errorf("gate-view scope_overlap = %+v, want the same block fishhawk_get_plan reported %+v", gv.GateView.ScopeOverlap, aOv)
	}

	// --- approval condition 1: a run-bound token for A reads neither block ---
	runTok := fetchMCPToken(t, ctx, httpSrv.URL, a.row.ID, a.priv)
	runSession := connectMCPClient(t, ctx, fx.mcpBinary, runTok, httpSrv.URL)
	boundOv, boundRaw := getPlanOverlap(runSession, a)
	if boundOv != nil {
		t.Errorf("run-bound fishhawk_get_plan scope_overlap = %+v, want absent", boundOv)
	}
	bgvRes, err := runSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_get_gate_view", Arguments: map[string]any{"run_id": a.row.ID.String()},
	})
	if err != nil {
		t.Fatalf("run-bound CallTool fishhawk_get_gate_view: %v", err)
	}
	if bgvRes.IsError {
		t.Fatalf("run-bound fishhawk_get_gate_view returned error: %s", toolContentString(t, bgvRes))
	}
	var bgv struct {
		GateView struct {
			ScopeOverlap *e2eScopeOverlap `json:"scope_overlap"`
		} `json:"gate_view"`
	}
	decodeStructured(t, bgvRes, &bgv)
	if bgv.GateView.ScopeOverlap != nil {
		t.Errorf("run-bound gate-view scope_overlap = %+v, want absent", bgv.GateView.ScopeOverlap)
	}
	bgvRaw, _ := json.Marshal(bgvRes.StructuredContent)
	for _, leak := range []string{b.row.ID.String(), c.row.ID.String(), b.ref, c.ref} {
		if strings.Contains(boundRaw, leak) {
			t.Errorf("run-bound fishhawk_get_plan leaks %q", leak)
		}
		if strings.Contains(string(bgvRaw), leak) {
			t.Errorf("run-bound fishhawk_get_gate_view leaks %q", leak)
		}
	}
}
