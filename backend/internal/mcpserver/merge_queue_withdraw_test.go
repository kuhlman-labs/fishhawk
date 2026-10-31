package mcpserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The fishhawk_withdraw_merge_intent tool (ADR-112 R3, E87.17 / #4685) over a
// fake backend serving the withdraw route's RAW wire bytes, written in the
// server's json-tag spelling (backend/internal/server/merge_queue_withdraw.go
// mergeQueueWithdrawResponse) — never a re-encoding of the client mirror, so a
// tag drift surfaces as an absent field here.

const mqWithdrawRunID = "dddddddd-0000-0000-0000-000000000017"

const mqWithdrawWireBody = `{"run_id":"` + mqWithdrawRunID + `","entry_id":"eeeeeeee-0000-0000-0000-000000000017",
 "repo":"x/y","base_ref":"main","withdrawn":true,"settled_state":"dropped","settle_reason":"intent_withdrawn",
 "intent_sequence":31,"yield_reason":"forge_approval_absent",
 "live_run_id":"aaaaaaaa-0000-0000-0000-000000000001","audit_sequence":57}`

// mqWithdrawFake serves POST /v0/runs/{run_id}/merge-queue/withdraw with a
// fixed status + body and records each request's path and body.
type mqWithdrawFake struct {
	mu     sync.Mutex
	status int
	body   string
	paths  []string
	bodies []string
}

func newMQWithdrawFake(t *testing.T, fb *mqWithdrawFake) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/merge-queue/withdraw", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fb.mu.Lock()
		fb.paths = append(fb.paths, r.URL.Path)
		fb.bodies = append(fb.bodies, string(raw))
		status, body := fb.status, fb.body
		fb.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (fb *mqWithdrawFake) calls() ([]string, []string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]string(nil), fb.paths...), append([]string(nil), fb.bodies...)
}

// TestWithdrawMergeIntent_ValidatesAndPassesThrough: a bad UUID and a blank
// reason are refused BEFORE any HTTP call; a valid call posts the trimmed
// {"reason"} to the run's withdraw route, passes every wire field through,
// and names fishhawk_merge_run on the run as its next_action.
func TestWithdrawMergeIntent_ValidatesAndPassesThrough(t *testing.T) {
	fb := &mqWithdrawFake{body: mqWithdrawWireBody}
	r := newMergeRunResolver(newMQWithdrawFake(t, fb))
	ctx := context.Background()

	for _, in := range []withdrawMergeIntentInput{
		{RunID: "not-a-uuid", Reason: "x"},
		{RunID: "", Reason: "x"},
		{RunID: mqWithdrawRunID, Reason: ""},
		{RunID: mqWithdrawRunID, Reason: " \t "},
	} {
		if _, _, err := r.withdrawMergeIntent(ctx, nil, in); err == nil {
			t.Errorf("input %+v: err = nil, want a local refusal", in)
		}
	}
	if paths, _ := fb.calls(); len(paths) != 0 {
		t.Fatalf("backend called %d times on locally refused input, want 0", len(paths))
	}

	_, out, err := r.withdrawMergeIntent(ctx, nil, withdrawMergeIntentInput{RunID: " " + mqWithdrawRunID + " ", Reason: "  merge it inline  "})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	paths, bodies := fb.calls()
	if len(paths) != 1 || paths[0] != "/v0/runs/"+mqWithdrawRunID+"/merge-queue/withdraw" || bodies[0] != `{"reason":"merge it inline"}` {
		t.Fatalf("requests = %v %v, want one POST to the run's withdraw route with the trimmed reason", paths, bodies)
	}
	res := out.Result
	if res.RunID != mqWithdrawRunID || res.EntryID != "eeeeeeee-0000-0000-0000-000000000017" || res.Repo != "x/y" ||
		res.BaseRef != "main" || !res.Withdrawn || res.SettledState != "dropped" || res.SettleReason != "intent_withdrawn" ||
		res.IntentSequence != 31 || res.YieldReason != "forge_approval_absent" ||
		res.LiveRunID != "aaaaaaaa-0000-0000-0000-000000000001" || res.AuditSequence != 57 {
		t.Fatalf("result = %+v, want every wire field passed through", res)
	}
	for _, want := range []string{"57", "intent_withdrawn", "fishhawk_merge_run", "nothing was merged"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message = %q, want it to name %q", out.Message, want)
		}
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_merge_run" || na.Params["run_id"] != mqWithdrawRunID {
		t.Fatalf("next_action = %+v, want fishhawk_merge_run on run %s", na, mqWithdrawRunID)
	}
}

// TestWithdrawMergeIntent_RunIDFallback: a body that omits run_id still names
// the requested run in the result and the next_action.
func TestWithdrawMergeIntent_RunIDFallback(t *testing.T) {
	fb := &mqWithdrawFake{body: `{"entry_id":"e","repo":"x/y","base_ref":"main","withdrawn":true,"settled_state":"dropped","settle_reason":"intent_withdrawn","yield_reason":"forge_approval_absent","audit_sequence":3}`}
	r := newMergeRunResolver(newMQWithdrawFake(t, fb))
	_, out, err := r.withdrawMergeIntent(context.Background(), nil, withdrawMergeIntentInput{RunID: mqWithdrawRunID, Reason: "x"})
	if err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if out.Result.RunID != mqWithdrawRunID || out.NextAction == nil || out.NextAction.Params["run_id"] != mqWithdrawRunID {
		t.Fatalf("output = %+v, want the requested run id filled in", out)
	}
}

// TestWithdrawMergeIntent_RefusalIsToolErrorWithHint: each backend refusal is
// a TOOL ERROR naming the code, with the remedy hint where one exists, and no
// next_action.
func TestWithdrawMergeIntent_RefusalIsToolErrorWithHint(t *testing.T) {
	for _, tc := range []struct {
		status    int
		code      string
		wantInErr string
	}{
		{http.StatusConflict, "merge_queue_entry_not_yielded", "cancel the run"},
		{http.StatusConflict, "merge_dispatch_claimed", "details.next_step"},
		{http.StatusConflict, "merge_queue_entry_live", "never withdrawn mid-pass"},
		{http.StatusConflict, "merge_queue_not_queued", "already withdrawn"},
		{http.StatusConflict, "merge_queue_no_intent", "no merge intent"},
		{http.StatusServiceUnavailable, "merge_lock_unavailable", "re-invoke"},
		{http.StatusForbidden, "run_token_forbidden", "operator session"},
		{http.StatusForbidden, "insufficient_scope", "write:runs"},
		{http.StatusNotFound, "run_not_found", "run_not_found"},
		{http.StatusInternalServerError, "merge_queue_withdraw_failed", "merge_queue_withdraw_failed"},
		{http.StatusServiceUnavailable, "merge_queue_unconfigured", "merge_queue_unconfigured"},
	} {
		fb := &mqWithdrawFake{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
		r := newMergeRunResolver(newMQWithdrawFake(t, fb))
		_, out, err := r.withdrawMergeIntent(context.Background(), nil, withdrawMergeIntentInput{RunID: mqWithdrawRunID, Reason: "x"})
		if err == nil || !strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), tc.wantInErr) {
			t.Errorf("%s: err = %v, want a tool error naming %s and %q", tc.code, err, tc.code, tc.wantInErr)
		}
		if out.NextAction != nil || out.Result.Withdrawn {
			t.Errorf("%s: output %+v on a refusal, want empty", tc.code, out)
		}
	}
	if got := withdrawMergeIntentRefusalHint("run_not_found"); got != "" {
		t.Errorf("hint(run_not_found) = %q, want none (passed through verbatim)", got)
	}
}

// TestWithdrawMergeIntent_RegisteredDescription: the registered tool leads
// with WHEN, states the eligibility, and the SDK validates the structured
// output over a real session.
func TestWithdrawMergeIntent_RegisteredDescription(t *testing.T) {
	fb := &mqWithdrawFake{body: mqWithdrawWireBody}
	srv := newMQWithdrawFake(t, fb)
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerWithdrawMergeIntent(server, newMergeRunResolver(srv))
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	tools, err := cs.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 {
		t.Fatalf("list tools = %v, %v; want one", tools, err)
	}
	desc := tools.Tools[0].Description
	if !strings.HasPrefix(desc, "Use this when") {
		t.Errorf("description must lead with WHEN; got %q", desc[:min(len(desc), 80)])
	}
	for _, want := range []string{"ELIGIBILITY", "YIELDED", "#4685", "merge_dispatch_claimed", "merge_queue_entry_not_yielded",
		"merge_queue_entry_live", "merge_queue_intent_withdrawn", "fishhawk_merge_run", "write:runs", "run_token_forbidden"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q", want)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_withdraw_merge_intent",
		Arguments: map[string]any{"run_id": mqWithdrawRunID, "reason": "x"}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool = %+v, %v; want a structured result", res, err)
	}
}
