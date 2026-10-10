package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// TestE2E_ResolveConcerns_Addressed is the E83.53 / #4086 cross-boundary seam:
// the real fishhawk-mcp binary routes an operator concern back with
// fishhawk_fixup_stage carrying operator_evidence (minting a durable
// addressed_pending row the operator_evidence_routed veto protects), then
// resolves it with fishhawk_resolve_concerns against a server that carries a
// REAL Postgres ConcernRepo (binding condition 2) — so the addressed_pending ->
// addressed transition happens in the real store, not a fake. It reads the
// concern state and the audit chain back: the concern is `addressed` (NOT
// waived), with exactly one concern_resolved_with_evidence row carrying the
// evidence and zero concern_waived rows.
func TestE2E_ResolveConcerns_Addressed(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	concernRepo := concern.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		ConcernRepo:  concernRepo,
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            fx.runID,
		Sequence:         1,
		Type:             runpkg.StageTypeImplement,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	// 1. Route an operator concern back WITH operator_evidence: the real path
	// that leaves a concern addressed_pending and immune to reviewer
	// auto-resolve.
	routed, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_fixup_stage",
		Arguments: map[string]any{
			"stage_id":          stage.ID.String(),
			"operator_concern":  "the merge path dereferences a nil PR head; guard it",
			"operator_evidence": "reproduced locally: go test -run TestMergeNilHead panics",
			"reason":            "operator reproduced a nil deref the reviewer retired",
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_fixup_stage: %v", err)
	}
	if routed.IsError {
		t.Fatalf("fixup tool returned error: %s", toolContentString(t, routed))
	}
	rows, err := concernRepo.ListByRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 1 || rows[0].State != concern.StateAddressedPending {
		t.Fatalf("concern rows = %+v, want exactly one addressed_pending operator concern", rows)
	}
	concernID := rows[0].ID

	// 2. Resolve it through the real binary.
	const evidence = "re-ran TestMergeNilHead at the fix-up head; it passes"
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_resolve_concerns",
		Arguments: map[string]any{
			"run_id":      fx.runID.String(),
			"concern_ids": []string{concernID.String()},
			"evidence":    evidence,
		},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_resolve_concerns: %v", err)
	}
	if result.IsError {
		t.Fatalf("resolve tool returned error: %s", toolContentString(t, result))
	}
	var out struct {
		Result struct {
			Resolved int `json:"resolved"`
			Failed   int `json:"failed"`
			Results  []struct {
				ConcernID string `json:"concern_id"`
				Applied   bool   `json:"applied"`
				State     string `json:"state"`
			} `json:"results"`
		} `json:"result"`
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode tool output: %v\n%s", err, raw)
	}
	if out.Result.Resolved != 1 || out.Result.Failed != 0 || len(out.Result.Results) != 1 ||
		out.Result.Results[0].ConcernID != concernID.String() || !out.Result.Results[0].Applied ||
		out.Result.Results[0].State != string(concern.StateAddressed) {
		t.Errorf("tool output = %+v, want one applied addressed result for %s", out.Result, concernID)
	}

	// 3. Committed state: the REAL store holds `addressed`, not waived.
	got, err := concernRepo.GetByIDs(ctx, []uuid.UUID{concernID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if got[0].State != concern.StateAddressed {
		t.Errorf("stored state = %q, want addressed (not waived)", got[0].State)
	}
	if got[0].StateReason != "operator evidence: "+evidence {
		t.Errorf("stored state_reason = %q, want the prefixed evidence", got[0].StateReason)
	}

	// 4. The audit chain: one intent row carrying the evidence, no waiver, no
	// corrective.
	intents, err := auditRepo.ListForRunByCategory(ctx, fx.runID, server.CategoryConcernResolvedWithEvidence)
	if err != nil {
		t.Fatalf("list concern_resolved_with_evidence: %v", err)
	}
	if len(intents) != 1 {
		t.Fatalf("concern_resolved_with_evidence entries = %d, want 1", len(intents))
	}
	var payload struct {
		ConcernID  string `json:"concern_id"`
		PriorState string `json:"prior_state"`
		Evidence   string `json:"evidence"`
	}
	if err := json.Unmarshal(intents[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.ConcernID != concernID.String() || payload.PriorState != string(concern.StateAddressedPending) || payload.Evidence != evidence {
		t.Errorf("payload = %+v, want the concern, prior addressed_pending and the evidence", payload)
	}
	for _, cat := range []string{server.CategoryConcernWaived, server.CategoryConcernResolveFailed} {
		entries, err := auditRepo.ListForRunByCategory(ctx, fx.runID, cat)
		if err != nil {
			t.Fatalf("list %s: %v", cat, err)
		}
		if len(entries) != 0 {
			t.Errorf("%s entries = %d, want 0", cat, len(entries))
		}
	}
}
