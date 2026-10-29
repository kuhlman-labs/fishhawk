package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// TestMCP_RecordEscape_EndToEnd is the E94.3 / #4694 cross-boundary seam: the
// real fishhawk-mcp binary -> POST /v0/runs/{run_id}/escapes -> the handler ->
// the Postgres audit chain (through the production DedupedChainAppender),
// on an acme/widgets run with a STATIC operator api token:
//
//  1. a human_finding RECORD is born 201 pending_judgement with
//     pending_reason credential_not_forge_verified (a static token is not a
//     forge-verified credential, ADR-117 A1.2);
//  2. its replay is refused escape_already_recorded and writes nothing;
//  3. a CLOSE with the same token is refused
//     escape_attestation_requires_forge_credential (A1.3) and writes nothing;
//  4. fishhawk_list_audit reads back exactly ONE escape_recorded row.
func TestMCP_RecordEscape_EndToEnd(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    auditRepo,
		SigningRepo:  signing.NewPostgresRepository(fx.pool),
		APITokenRepo: fx.apitokenRepo,
		GitHub:       githubclient.New(nil),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	requiresCharter := false
	rn, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:            "acme/widgets",
		WorkflowID:      "feature_change",
		WorkflowSHA:     "deadbeef",
		TriggerSource:   runpkg.TriggerCLI,
		RequiresCharter: &requiresCharter,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)

	escapeRows := func() []*audit.Entry {
		t.Helper()
		rows, err := auditRepo.ListForRunByCategory(ctx, rn.ID, server.CategoryEscapeRecorded)
		if err != nil {
			t.Fatalf("list escape_recorded: %v", err)
		}
		return rows
	}
	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		args["run_id"] = rn.ID.String()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_record_escape", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool fishhawk_record_escape: %v", err)
		}
		return res
	}
	record := map[string]any{
		"source": "human_finding", "severity": "high", "category": "correctness",
		"description":     "the widget sorter drops the last element on an even-length input",
		"files":           []string{"internal/sort/sort.go"},
		"head_sha":        strings.Repeat("a", 40),
		"idempotency_key": "widget-sort-escape-1",
	}

	// 1. Birth.
	res := call(record)
	if res.IsError {
		t.Fatalf("record returned a tool error: %s", toolContentString(t, res))
	}
	var born struct {
		Result struct {
			AuditSequence  int64  `json:"audit_sequence"`
			RecordKind     string `json:"record_kind"`
			State          string `json:"state"`
			JudgementBasis string `json:"judgement_basis"`
			PendingReason  string `json:"pending_reason"`
			SourceEventKey string `json:"source_event_key"`
		} `json:"result"`
	}
	decodeStructured(t, res, &born)
	if born.Result.RecordKind != "birth" || born.Result.State != "pending_judgement" ||
		born.Result.PendingReason != "credential_not_forge_verified" || born.Result.JudgementBasis != "awaiting_attestation" ||
		born.Result.SourceEventKey != "idempotency_key:widget-sort-escape-1" || born.Result.AuditSequence <= 0 {
		t.Fatalf("birth = %+v, want a pending_judgement birth (credential_not_forge_verified) keyed idempotency_key:widget-sort-escape-1", born.Result)
	}
	rows := escapeRows()
	if len(rows) != 1 || rows[0].Sequence != born.Result.AuditSequence {
		t.Fatalf("escape_recorded rows = %d, want 1 at sequence %d", len(rows), born.Result.AuditSequence)
	}
	var payload struct {
		RecordKind string `json:"record_kind"`
		State      string `json:"state"`
		Recorder   struct {
			CredentialKind string `json:"credential_kind"`
		} `json:"recorder"`
		Escape struct {
			Files []string `json:"files"`
		} `json:"escape"`
	}
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("decode birth payload: %v", err)
	}
	if payload.RecordKind != "birth" || payload.State != "pending_judgement" || payload.Recorder.CredentialKind != "static" ||
		len(payload.Escape.Files) != 1 || payload.Escape.Files[0] != "internal/sort/sort.go" {
		t.Errorf("persisted birth = %+v, want a pending static-credential birth carrying the file", payload)
	}

	// 2. Replay: 409, nothing written.
	res = call(record)
	if !res.IsError || !strings.Contains(toolContentString(t, res), "escape_already_recorded") {
		t.Fatalf("replay = %s, want a tool error naming escape_already_recorded", toolContentString(t, res))
	}
	if n := len(escapeRows()); n != 1 {
		t.Fatalf("escape_recorded rows after replay = %d, want 1", n)
	}

	// 3. Close with the same static token: the attestation needs a
	// forge-verified credential.
	res = call(map[string]any{
		"supersedes_sequence": born.Result.AuditSequence,
		"attestation":         map[string]any{"judgement": "defect"},
	})
	if !res.IsError || !strings.Contains(toolContentString(t, res), "escape_attestation_requires_forge_credential") {
		t.Fatalf("close = %s, want a tool error naming escape_attestation_requires_forge_credential", toolContentString(t, res))
	}
	if n := len(escapeRows()); n != 1 {
		t.Fatalf("escape_recorded rows after the refused close = %d, want 1", n)
	}

	// 4. Read back through the MCP audit read.
	listRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_list_audit",
		Arguments: map[string]any{"run_id": rn.ID.String(), "category": server.CategoryEscapeRecorded}})
	if err != nil {
		t.Fatalf("CallTool fishhawk_list_audit: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("fishhawk_list_audit(escape_recorded) returned a tool error: %s", toolContentString(t, listRes))
	}
	var listed struct {
		Items []struct {
			Category string `json:"category"`
			Sequence int64  `json:"sequence"`
		} `json:"items"`
	}
	decodeStructured(t, listRes, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Sequence != born.Result.AuditSequence || listed.Items[0].Category != server.CategoryEscapeRecorded {
		t.Errorf("fishhawk_list_audit items = %+v, want exactly the one birth at sequence %d", listed.Items, born.Result.AuditSequence)
	}
}
