package mcpe2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// promoteHost is the host label every marker POST carries, so all four stages
// share the default local-implement:<host> group (limit 1).
const promoteHost = "e2e-promote-h1"

// parkedImplementStage creates a run with one implement stage parked at
// awaiting_host_dispatch, the state the host-dispatch marker admits or queues.
func parkedImplementStage(t *testing.T, ctx context.Context, fx *e2eFixture) *runpkg.Stage {
	t.Helper()
	requiresCharter := false
	r, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
		TriggerSource: runpkg.TriggerCLI, RequiresCharter: &requiresCharter,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	st, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage: %v", err)
	}
	if st, err = fx.runRepo.TransitionStage(ctx, st.ID, runpkg.StageStateAwaitingHostDispatch, nil); err != nil {
		t.Fatalf("park stage: %v", err)
	}
	return st
}

// hostDispatchMarker POSTs the real host-dispatch marker for st with the
// operator bearer and returns the status, the error code ("" on a 2xx) and
// the queued answer's details.position.
func hostDispatchMarker(t *testing.T, ctx context.Context, url, token string, st *runpkg.Stage) (int, string, int) {
	t.Helper()
	target := url + "/v0/runs/" + st.RunID.String() + "/stages/" + st.ID.String() + "/host-dispatch"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader([]byte(`{"host":"`+promoteHost+`"}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Position int `json:"position"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return resp.StatusCode, e.Error.Code, e.Error.Details.Position
}

// promoteRunStatus is the slice of fishhawk_get_run_status this test reads.
// The concurrency block decodes through the MCP binary's StageConcurrency
// mirror, so a `promoted` tag drift there re-encodes the flag as absent.
type promoteRunStatus struct {
	Stages []struct {
		ID          string `json:"id"`
		Concurrency *struct {
			Status   string `json:"status"`
			Position int    `json:"position"`
			Promoted bool   `json:"promoted"`
		} `json:"concurrency"`
	} `json:"stages"`
}

// concurrencyOf reads stage st's concurrency block through the real
// fishhawk_get_run_status tool.
func concurrencyOf(t *testing.T, ctx context.Context, session *mcp.ClientSession, st *runpkg.Stage) (status string, position int, promoted bool) {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": st.RunID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_get_run_status tool error: %s", toolContentString(t, res))
	}
	var out promoteRunStatus
	decodeStructured(t, res, &out)
	for _, s := range out.Stages {
		if s.ID == st.ID.String() {
			if s.Concurrency == nil {
				t.Fatalf("stage %s has no concurrency block on fishhawk_get_run_status", st.ID)
			}
			return s.Concurrency.Status, s.Concurrency.Position, s.Concurrency.Promoted
		}
	}
	t.Fatalf("stage %s not on fishhawk_get_run_status", st.ID)
	return "", 0, false
}

// TestE2E_PromoteQueuedStage is the #4191 end-to-end done-means: the REAL
// fishhawk-mcp binary -> the real promote route -> the Postgres concurrency
// store -> the audit chain -> the stage read surface -> the host-dispatch
// marker. Holder H is admitted and A, B, C queue behind it in that order;
// promoting C moves it 3 -> 1, fishhawk_get_run_status shows C at position 1
// promoted:true (A at 2), one stage_concurrency_promoted row carries the
// payload, and once H settles the marker queues A behind C and admits C.
func TestE2E_PromoteQueuedStage(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	srv := server.New(server.Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runRepo,
		AuditRepo:    audit.NewPostgresRepository(fx.pool),
		APITokenRepo: fx.apitokenRepo,
		Concurrency:  concurrency.NewPostgresStore(fx.pool),
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	url := httpSrv.URL

	h := parkedImplementStage(t, ctx, fx)
	if status, code, _ := hostDispatchMarker(t, ctx, url, fx.operatorTok, h); status != http.StatusOK {
		t.Fatalf("marker H = %d %q, want 200 admitted", status, code)
	}
	waiters := make([]*runpkg.Stage, 3)
	for i := range waiters {
		waiters[i] = parkedImplementStage(t, ctx, fx)
		status, code, pos := hostDispatchMarker(t, ctx, url, fx.operatorTok, waiters[i])
		if status != http.StatusConflict || code != "concurrency_slot_queued" || pos != i+1 {
			t.Fatalf("marker waiter %d = %d %q position %d, want 409 concurrency_slot_queued position %d", i, status, code, pos, i+1)
		}
	}
	a, c := waiters[0], waiters[2]

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, url)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_promote_queued_stage",
		Arguments: map[string]any{"stage_id": c.ID.String(), "reason": "C fixes the flake the others fail on"},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_promote_queued_stage: %v", err)
	}
	if res.IsError {
		t.Fatalf("fishhawk_promote_queued_stage tool error: %s", toolContentString(t, res))
	}
	var out struct {
		Result struct {
			StageID       string `json:"stage_id"`
			RunID         string `json:"run_id"`
			Group         string `json:"group"`
			Limit         int    `json:"limit"`
			Promoted      bool   `json:"promoted"`
			OldPosition   int    `json:"old_position"`
			NewPosition   int    `json:"new_position"`
			AuditSequence *int64 `json:"audit_sequence"`
			Holders       []struct {
				StageID string `json:"stage_id"`
			} `json:"holders"`
		} `json:"result"`
		NextAction *struct {
			Action string            `json:"action"`
			Params map[string]string `json:"params"`
		} `json:"next_action"`
	}
	decodeStructured(t, res, &out)
	r := out.Result
	if !r.Promoted || r.OldPosition != 3 || r.NewPosition != 1 || r.StageID != c.ID.String() || r.RunID != c.RunID.String() ||
		r.Group != "local-implement:"+promoteHost || r.Limit != 1 || r.AuditSequence == nil ||
		len(r.Holders) != 1 || r.Holders[0].StageID != h.ID.String() {
		t.Fatalf("promote result = %+v, want C promoted 3 -> 1 behind holder H with an audit sequence", r)
	}
	if na := out.NextAction; na == nil || na.Action != "fishhawk_await_stage" || na.Params["stage_id"] != c.ID.String() ||
		na.Params["run_id"] != c.RunID.String() || na.Params["stage"] != "implement" {
		t.Errorf("next_action = %+v, want fishhawk_await_stage on C (stage=implement)", out.NextAction)
	}

	// The read surface: C first and promoted, A second and not.
	if status, pos, promoted := concurrencyOf(t, ctx, session, c); status != "awaiting_concurrency_slot" || pos != 1 || !promoted {
		t.Errorf("C concurrency = %s position %d promoted %v, want awaiting_concurrency_slot position 1 promoted true", status, pos, promoted)
	}
	if status, pos, promoted := concurrencyOf(t, ctx, session, a); status != "awaiting_concurrency_slot" || pos != 2 || promoted {
		t.Errorf("A concurrency = %s position %d promoted %v, want awaiting_concurrency_slot position 2 promoted false", status, pos, promoted)
	}

	// The audit chain: exactly one stage_concurrency_promoted row, on C.
	rows, err := fx.pool.Query(ctx, `SELECT sequence, run_id, stage_id, actor_subject, payload FROM audit_entries WHERE category = 'stage_concurrency_promoted'`)
	if err != nil {
		t.Fatalf("query promoted rows: %v", err)
	}
	type promotedRow struct {
		seq     int64
		runID   uuid.UUID
		stageID *uuid.UUID
		subject *string
		payload map[string]any
	}
	var got []promotedRow
	for rows.Next() {
		var pr promotedRow
		var raw []byte
		if err := rows.Scan(&pr.seq, &pr.runID, &pr.stageID, &pr.subject, &raw); err != nil {
			t.Fatalf("scan promoted row: %v", err)
		}
		if err := json.Unmarshal(raw, &pr.payload); err != nil {
			t.Fatalf("decode promoted payload: %v", err)
		}
		got = append(got, pr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate promoted rows: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stage_concurrency_promoted rows = %d, want exactly 1: %+v", len(got), got)
	}
	pr := got[0]
	if pr.seq != *r.AuditSequence || pr.runID != c.RunID || pr.stageID == nil || *pr.stageID != c.ID || pr.subject == nil || *pr.subject == "" {
		t.Errorf("promoted row = %+v, want sequence %d on run/stage C with an actor subject", pr, *r.AuditSequence)
	}
	for k, want := range map[string]any{
		"stage_id": c.ID.String(), "run_id": c.RunID.String(), "group": "local-implement:" + promoteHost,
		"limit": float64(1), "old_position": float64(3), "new_position": float64(1),
		"reason": "C fixes the flake the others fail on",
	} {
		if pr.payload[k] != want {
			t.Errorf("payload[%s] = %v, want %v", k, pr.payload[k], want)
		}
	}
	if pr.subject != nil && pr.payload["actor"] != *pr.subject {
		t.Errorf("payload actor = %v, want the row's actor subject %q", pr.payload["actor"], *pr.subject)
	}

	// H settles; A polls FIRST and still queues behind C, then C is admitted.
	for _, to := range []runpkg.StageState{runpkg.StageStateRunning, runpkg.StageStateSucceeded} {
		if _, err := fx.runRepo.TransitionStage(ctx, h.ID, to, nil); err != nil {
			t.Fatalf("settle H -> %s: %v", to, err)
		}
	}
	if status, code, pos := hostDispatchMarker(t, ctx, url, fx.operatorTok, a); status != http.StatusConflict || code != "concurrency_slot_queued" || pos != 2 {
		t.Errorf("marker A after H settled = %d %q position %d, want 409 concurrency_slot_queued position 2 (C was promoted ahead)", status, code, pos)
	}
	if status, code, _ := hostDispatchMarker(t, ctx, url, fx.operatorTok, c); status != http.StatusOK {
		t.Errorf("marker C after H settled = %d %q, want 200 admitted", status, code)
	}
	if st, err := fx.runRepo.GetStage(ctx, c.ID); err != nil || st.State != runpkg.StageStateDispatched {
		t.Errorf("C after the marker = %v (err %v), want dispatched", st, err)
	}
}
