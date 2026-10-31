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
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// TestE2E_WithdrawMergeIntent is the E87.17 / #4685 (ADR-112 R3)
// cross-boundary done-means for the withdraw-intent verb: the REAL
// fishhawk-mcp binary -> POST /v0/runs/{id}/merge-queue/withdraw -> the
// Postgres mergequeue store (the yielded entry settled dropped
// intent_withdrawn inside one transaction with the
// merge_queue_intent_withdrawn append) -> the audit chain -> the two reads
// back out (fishhawk_get_merge_queue and fishhawk_list_audit). A holds the
// line live; B is held, YIELDED, with merge intent. Withdrawing B answers
// withdrawn:true, the queue read lists A only, and one
// merge_queue_intent_withdrawn row carries the payload (without the
// untrusted yield_detail). A second withdraw of B is a merge_queue_not_queued
// tool error and a withdraw of live A is merge_queue_entry_live, both over
// the wire. A json-tag drift between server/merge_queue_withdraw.go and the
// mcpserver mirror fails here.
func TestE2E_WithdrawMergeIntent(t *testing.T) {
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

	const (
		repo, base  = "x/withdraw", "main"
		yieldReason = "forge_approval_absent"
		yieldDetail = "UNTRUSTED-YIELD-DETAIL-4685"
		intentSeq   = int64(29)
	)
	requiresCharter := false
	runs := make([]*runpkg.Run, 0, 2)
	for range 2 {
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
		runs = append(runs, r)
	}
	a, b := runs[0], runs[1]
	if _, err := store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: a.ID, BaseRef: base, HeadSHA: "a0", MergeIntent: true}); err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}
	if adm, err := store.AdmitNext(ctx, mergequeue.Base{Repo: repo, BaseRef: base}); err != nil || !adm.Admitted || adm.Entry.RunID != a.ID {
		t.Fatalf("AdmitNext = %+v, %v; want A admitted live", adm, err)
	}
	// B is seeded held + YIELDED + with intent BY CONSTRUCTION through the
	// store (the state a forge_approval_absent yield leaves).
	if _, err := store.Enqueue(ctx, mergequeue.EnqueueRequest{RunID: b.ID, BaseRef: base, HeadSHA: "b0",
		MergeIntent: true, IntentSequence: intentSeq,
		Yield: &mergequeue.YieldMarker{Reason: yieldReason, Detail: yieldDetail}}); err != nil {
		t.Fatalf("Enqueue B yielded: %v", err)
	}
	bBefore, found, err := store.ActiveForRun(ctx, b.ID)
	if err != nil || !found || bBefore.Entry.State != mergequeue.StateHeld || !bBefore.Entry.Yielded() || bBefore.Entry.MergeIntentAt == nil {
		t.Fatalf("B before withdraw = %+v found=%v err=%v, want held, yielded, with intent", bBefore, found, err)
	}

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, httpSrv.URL)
	const reason = "B's approval will not come; merge it inline"
	var out struct {
		Result struct {
			RunID          string `json:"run_id"`
			EntryID        string `json:"entry_id"`
			Repo           string `json:"repo"`
			BaseRef        string `json:"base_ref"`
			Withdrawn      bool   `json:"withdrawn"`
			SettledState   string `json:"settled_state"`
			SettleReason   string `json:"settle_reason"`
			IntentSequence int64  `json:"intent_sequence"`
			YieldReason    string `json:"yield_reason"`
			LiveRunID      string `json:"live_run_id"`
			AuditSequence  int64  `json:"audit_sequence"`
		} `json:"result"`
		NextAction *struct {
			Action string            `json:"action"`
			Params map[string]string `json:"params"`
		} `json:"next_action"`
	}
	raw := mqeCall(t, ctx, session, "fishhawk_withdraw_merge_intent", map[string]any{"run_id": b.ID.String(), "reason": reason}, &out)
	r := out.Result
	if !r.Withdrawn || r.RunID != b.ID.String() || r.EntryID != bBefore.Entry.ID.String() || r.Repo != repo || r.BaseRef != base ||
		r.SettledState != string(mergequeue.StateDropped) || r.SettleReason != mergequeue.ReasonIntentWithdrawn ||
		r.IntentSequence != intentSeq || r.YieldReason != yieldReason || r.LiveRunID != a.ID.String() || r.AuditSequence <= 0 {
		t.Fatalf("withdraw result = %+v, want B withdrawn (dropped intent_withdrawn) behind live A with an audit sequence\nraw: %s", r, raw)
	}
	if na := out.NextAction; na == nil || na.Action != "fishhawk_merge_run" || na.Params["run_id"] != b.ID.String() {
		t.Errorf("next_action = %+v, want fishhawk_merge_run on B", out.NextAction)
	}

	// Committed state: B has no active entry; its newest row is the dropped one.
	if st, found, err := store.ActiveForRun(ctx, b.ID); err != nil || found {
		t.Fatalf("B ActiveForRun after withdraw = %+v found=%v err=%v, want no active entry", st, found, err)
	}

	// Read 1: the queue through fishhawk_get_merge_queue lists A only.
	var q struct {
		Bases []struct {
			BaseRef string `json:"base_ref"`
			Entries []struct {
				RunID string `json:"run_id"`
				State string `json:"state"`
			} `json:"entries"`
		} `json:"bases"`
	}
	raw = mqeCall(t, ctx, session, "fishhawk_get_merge_queue", map[string]any{"repo": repo, "base_ref": base}, &q)
	if len(q.Bases) != 1 || len(q.Bases[0].Entries) != 1 || q.Bases[0].Entries[0].RunID != a.ID.String() || q.Bases[0].Entries[0].State != "live" {
		t.Fatalf("fishhawk_get_merge_queue = %+v, want one base listing live A only\nraw: %s", q, raw)
	}

	// Read 2: the merge_queue_intent_withdrawn row through fishhawk_list_audit.
	listRes, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_list_audit",
		Arguments: map[string]any{"run_id": b.ID.String(), "category": server.CategoryMergeQueueIntentWithdrawn}})
	if err != nil {
		t.Fatalf("CallTool fishhawk_list_audit: %v", err)
	}
	if listRes.IsError {
		t.Fatalf("fishhawk_list_audit(merge_queue_intent_withdrawn) returned a tool error: %s", toolContentString(t, listRes))
	}
	var listed struct {
		Items []struct {
			Category string          `json:"category"`
			Sequence int64           `json:"sequence"`
			Payload  json.RawMessage `json:"payload"`
		} `json:"items"`
	}
	decodeStructured(t, listRes, &listed)
	if len(listed.Items) != 1 || listed.Items[0].Sequence != r.AuditSequence {
		t.Fatalf("fishhawk_list_audit items = %+v, want the one withdrawn row at sequence %d", listed.Items, r.AuditSequence)
	}
	var payload map[string]any
	if err := json.Unmarshal(listed.Items[0].Payload, &payload); err != nil {
		t.Fatalf("decode withdrawn payload: %v (%s)", err, listed.Items[0].Payload)
	}
	for k, w := range map[string]any{
		"entry_id": bBefore.Entry.ID.String(), "run_id": b.ID.String(), "repo": repo, "base_ref": base,
		"anchored_head_sha": "b0", "intent_sequence": float64(intentSeq), "yield_reason": yieldReason, "reason": reason,
	} {
		if payload[k] != w {
			t.Errorf("payload[%s] = %v, want %v", k, payload[k], w)
		}
	}
	if s, _ := payload["actor"].(string); s == "" {
		t.Errorf("payload actor = %v, want the operator subject", payload["actor"])
	}
	if _, has := payload["yield_detail"]; has || strings.Contains(string(listed.Items[0].Payload), yieldDetail) {
		t.Errorf("payload = %s, want no yield_detail (untrusted forge text)", listed.Items[0].Payload)
	}

	// Refusals over the wire: a second withdraw of B finds no active entry,
	// and live A is never withdrawn mid-pass.
	mqwCallRefused(t, ctx, session, b.ID.String(), "merge_queue_not_queued")
	mqwCallRefused(t, ctx, session, a.ID.String(), "merge_queue_entry_live")
	if st, found, err := store.ActiveForRun(ctx, a.ID); err != nil || !found || st.Entry.State != mergequeue.StateLive {
		t.Fatalf("A after the refused withdraw = %+v found=%v err=%v, want still live", st, found, err)
	}
	listRes, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_list_audit",
		Arguments: map[string]any{"run_id": a.ID.String(), "category": server.CategoryMergeQueueIntentWithdrawn}})
	if err != nil || listRes.IsError {
		t.Fatalf("fishhawk_list_audit(A) = %+v, %v", listRes, err)
	}
	var listedA struct {
		Items []json.RawMessage `json:"items"`
	}
	decodeStructured(t, listRes, &listedA)
	if len(listedA.Items) != 0 {
		t.Fatalf("A carries %d merge_queue_intent_withdrawn rows after a refusal, want 0", len(listedA.Items))
	}
}

// mqwCallRefused calls fishhawk_withdraw_merge_intent on runID and requires a
// tool error naming code.
func mqwCallRefused(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID, code string) {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_withdraw_merge_intent",
		Arguments: map[string]any{"run_id": runID, "reason": "retry"}})
	if err != nil {
		t.Fatalf("CallTool fishhawk_withdraw_merge_intent(%s): %v", runID, err)
	}
	if !res.IsError {
		t.Fatalf("fishhawk_withdraw_merge_intent(%s) succeeded, want a %s tool error: %s", runID, code, toolContentString(t, res))
	}
	if body := toolContentString(t, res); !strings.Contains(body, code) {
		t.Fatalf("fishhawk_withdraw_merge_intent(%s) error = %s, want it to name %s", runID, body, code)
	}
}
