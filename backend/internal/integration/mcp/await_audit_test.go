package mcpe2e_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// TestE2E_AwaitAudit_SequenceAnchoredWait drives the real chain the
// fishhawk_await_audit unit tests fake: tool handler → apiClient.
// ListRunAudit → GET /v0/runs/{id}/audit?since_sequence=N served by
// handleListRunAudit → Postgres audit repo. This is the cross-boundary
// seam test (#962): the unit layers on both sides pass with a
// since_sequence encode/parse mismatch between client.go and reads.go;
// only this test catches one.
//
// Shape mirrors the #894 stale-review race: a stale implement_reviewed
// entry sits BELOW the fixup_pushed anchor, the post-fix-up verdict is
// appended only AFTER the await call has started, and the tool must
// return the fresh entry — never the stale one — with its sequence
// strictly greater than the anchor.
func TestE2E_AwaitAudit_SequenceAnchoredWait(t *testing.T) {
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	// Seed the pre-call history: a stale verdict, then the anchor.
	stale, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     fx.runID,
		Timestamp: time.Now().UTC(),
		Category:  "implement_reviewed",
		Payload:   json.RawMessage(`{"verdict":"approve_with_concerns","note":"stale pre-fix-up verdict"}`),
	})
	if err != nil {
		t.Fatalf("append stale implement_reviewed: %v", err)
	}
	anchor, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     fx.runID,
		Timestamp: time.Now().UTC(),
		Category:  "fixup_pushed",
		Payload:   json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("append fixup_pushed anchor: %v", err)
	}
	if anchor.Sequence <= stale.Sequence {
		t.Fatalf("anchor sequence %d not strictly greater than prior entry's %d; per-run sequences must be strictly increasing",
			anchor.Sequence, stale.Sequence)
	}

	token := fetchMCPToken(t, ctx, fx.url, fx.runID, fx.signingPriv)
	session := connectMCPClient(t, ctx, fx.mcpBinary, token, fx.url)

	// Append the post-fix-up verdict only after the await call has had
	// time to start and take its empty fast-path read, so the poll loop —
	// not the fast path — is what observes it.
	go func() {
		time.Sleep(1 * time.Second)
		_, aerr := auditRepo.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID:     fx.runID,
			Timestamp: time.Now().UTC(),
			Category:  "implement_reviewed",
			Payload:   json.RawMessage(`{"verdict":"approve","note":"post-fix-up verdict"}`),
		})
		if aerr != nil {
			t.Errorf("append post-fix-up implement_reviewed: %v", aerr)
		}
	}()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "fishhawk_await_audit",
		Arguments: map[string]any{
			"run_id":          fx.runID.String(),
			"category":        "implement_reviewed",
			"since_sequence":  anchor.Sequence,
			"timeout_seconds": 60,
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned error: %+v", result.Content)
	}

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out struct {
		Status string `json:"status"`
		Entry  *struct {
			Sequence int64  `json:"sequence"`
			Category string `json:"category"`
		} `json:"entry"`
		LatestSequence int64 `json:"latest_sequence"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode await_audit output: %v\nraw: %s", err, raw)
	}

	if out.Status != "found" {
		t.Fatalf("status = %q, want found\nraw: %s", out.Status, raw)
	}
	if out.Entry == nil {
		t.Fatalf("entry missing on found status\nraw: %s", raw)
	}
	if out.Entry.Category != "implement_reviewed" {
		t.Errorf("entry category = %q, want implement_reviewed", out.Entry.Category)
	}
	// The anchored wait must resolve to the post-fix-up entry: strictly
	// past the anchor, and in particular not the stale verdict below it.
	if out.Entry.Sequence <= anchor.Sequence {
		t.Errorf("entry sequence = %d, want strictly > anchor %d (stale entry was %d)",
			out.Entry.Sequence, anchor.Sequence, stale.Sequence)
	}
	if out.LatestSequence != out.Entry.Sequence {
		t.Errorf("latest_sequence = %d, want the matched entry's sequence %d",
			out.LatestSequence, out.Entry.Sequence)
	}
}
