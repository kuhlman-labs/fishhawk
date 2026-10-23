package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// TestE2E_PromoteMergeQueueEntry is the E87.6 / #4297 (ADR-093 D2/D7)
// cross-boundary done-means for the merge-queue promote: the REAL
// fishhawk-mcp binary -> POST /v0/runs/{id}/merge-queue/promote -> the
// Postgres mergequeue store (order_key, inside one transaction with the
// merge_queue_promoted append) -> the audit chain -> the two reads back out
// (fishhawk_get_merge_queue over GET /v0/repos/{owner}/{name}/merge-queue,
// and fishhawk_list_audit). A holds the line live; B, C, D are held in that
// order. Promoting D moves it 3 -> 1, the queue read lists A, D, B, C, and
// one merge_queue_promoted row carries the payload. A json-tag drift between
// server/merge_queue_promote.go and the mcpserver mirror fails here.
func TestE2E_PromoteMergeQueueEntry(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	store := mergequeue.NewPostgresStore(fx.pool)
	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    audit.NewPostgresRepository(fx.pool),
		APITokenRepo: fx.apitokenRepo,
		MergeQueue:   store,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	const repo, base = "x/promote", "main"
	requiresCharter := false
	runs := make([]*runpkg.Run, 0, 4)
	for _, head := range []string{"a0", "b0", "c0", "d0"} {
		r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
			Repo: repo, WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: runpkg.TriggerCLI,
			RequiresCharter: &requiresCharter,
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		if _, err := fx.pool.Exec(ctx, `UPDATE runs SET state = 'running' WHERE id = $1`, r.ID); err != nil {
			t.Fatalf("set running: %v", err)
		}
		if _, err := store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: r.ID, BaseRef: base, HeadSHA: head}); err != nil {
			t.Fatalf("Enqueue %s: %v", head, err)
		}
		runs = append(runs, r)
	}
	a, b, c, d := runs[0], runs[1], runs[2], runs[3]
	if adm, err := store.AdmitNext(ctx, mergequeue.Base{Repo: repo, BaseRef: base}); err != nil || !adm.Admitted || adm.Entry.RunID != a.ID {
		t.Fatalf("AdmitNext = %+v, %v; want A admitted live", adm, err)
	}
	dStatus, found, err := store.ActiveForRun(ctx, d.ID)
	if err != nil || !found || dStatus.Position != 3 {
		t.Fatalf("D before promote = %+v found=%v err=%v, want held position 3", dStatus, found, err)
	}

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	const reason = "D fixes the defect B and C fail on"
	var out struct {
		Result struct {
			RunID         string `json:"run_id"`
			EntryID       string `json:"entry_id"`
			Repo          string `json:"repo"`
			BaseRef       string `json:"base_ref"`
			Promoted      bool   `json:"promoted"`
			OldPosition   int    `json:"old_position"`
			NewPosition   int    `json:"new_position"`
			LiveRunID     string `json:"live_run_id"`
			AuditSequence *int64 `json:"audit_sequence"`
		} `json:"result"`
		NextAction *struct {
			Action string            `json:"action"`
			Params map[string]string `json:"params"`
		} `json:"next_action"`
	}
	raw := mqeCall(t, ctx, session, "fishhawk_promote_merge_queue_entry", map[string]any{"run_id": d.ID.String(), "reason": reason}, &out)
	r := out.Result
	if !r.Promoted || r.OldPosition != 3 || r.NewPosition != 1 || r.RunID != d.ID.String() || r.EntryID != dStatus.Entry.ID.String() ||
		r.Repo != repo || r.BaseRef != base || r.LiveRunID != a.ID.String() || r.AuditSequence == nil {
		t.Fatalf("promote result = %+v, want D promoted 3 -> 1 behind live A with an audit sequence\nraw: %s", r, raw)
	}
	if na := out.NextAction; na == nil || na.Action != "fishhawk_get_merge_queue" || na.Params["repo"] != repo || na.Params["base_ref"] != base {
		t.Errorf("next_action = %+v, want fishhawk_get_merge_queue on %s %s", out.NextAction, repo, base)
	}

	// Read 1: the queue order back through fishhawk_get_merge_queue.
	var q struct {
		Bases []struct {
			BaseRef string `json:"base_ref"`
			Entries []struct {
				RunID    string `json:"run_id"`
				State    string `json:"state"`
				Position int    `json:"position"`
			} `json:"entries"`
		} `json:"bases"`
	}
	raw = mqeCall(t, ctx, session, "fishhawk_get_merge_queue", map[string]any{"repo": repo, "base_ref": base}, &q)
	if len(q.Bases) != 1 || len(q.Bases[0].Entries) != 4 {
		t.Fatalf("fishhawk_get_merge_queue = %+v, want one base with four entries\nraw: %s", q, raw)
	}
	want := []struct {
		id       string
		state    string
		position int
	}{{a.ID.String(), "live", 0}, {d.ID.String(), "held", 1}, {b.ID.String(), "held", 2}, {c.ID.String(), "held", 3}}
	for i, w := range want {
		e := q.Bases[0].Entries[i]
		if e.RunID != w.id || e.State != w.state || e.Position != w.position {
			t.Errorf("entry %d = %+v, want run %s %s position %d (order A, D, B, C)\nraw: %s", i, e, w.id, w.state, w.position, raw)
		}
	}

	// Read 2: the merge_queue_promoted row through fishhawk_list_audit.
	listRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_list_audit",
		Arguments: map[string]any{"run_id": d.ID.String(), "category": server.CategoryMergeQueuePromoted}})
	if err != nil {
		t.Fatalf("CallTool fishhawk_list_audit: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("fishhawk_list_audit(merge_queue_promoted) returned a tool error: %s", toolContentString(t, listRes))
	}
	var listed struct {
		Items []struct {
			Category string          `json:"category"`
			Sequence int64           `json:"sequence"`
			Payload  json.RawMessage `json:"payload"`
		} `json:"items"`
	}
	decodeStructured(t, listRes, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Sequence != *r.AuditSequence {
		t.Fatalf("fishhawk_list_audit items = %+v, want the one promoted row at sequence %d", listed.Items, *r.AuditSequence)
	}
	var payload map[string]any
	if err := json.Unmarshal(listed.Items[0].Payload, &payload); err != nil {
		t.Fatalf("decode promoted payload: %v (%s)", err, listed.Items[0].Payload)
	}
	for k, w := range map[string]any{
		"entry_id": dStatus.Entry.ID.String(), "run_id": d.ID.String(), "repo": repo, "base_ref": base,
		"old_position": float64(3), "new_position": float64(1), "reason": reason,
	} {
		if payload[k] != w {
			t.Errorf("payload[%s] = %v, want %v", k, payload[k], w)
		}
	}
	if s, _ := payload["actor"].(string); s == "" {
		t.Errorf("payload actor = %v, want the operator subject", payload["actor"])
	}
}
