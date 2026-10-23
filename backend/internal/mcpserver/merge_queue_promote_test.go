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

// The fishhawk_promote_merge_queue_entry tool (ADR-093 D2/D7, E87.6 / #4297)
// over a fake backend serving the promote route's RAW wire bytes, written in
// the server's json-tag spelling (backend/internal/server/
// merge_queue_promote.go) — never a re-encoding of the client mirror, so a tag
// drift surfaces as an absent field here.

const mqPromoteRunID = "dddddddd-0000-0000-0000-000000000004"

const mqPromoteWireBody = `{"run_id":"` + mqPromoteRunID + `","entry_id":"eeeeeeee-0000-0000-0000-000000000004",
 "repo":"x/y","base_ref":"main","promoted":true,"old_position":3,"new_position":1,
 "live_run_id":"aaaaaaaa-0000-0000-0000-000000000001","audit_sequence":42}`

// mqPromoteFake serves POST /v0/runs/{run_id}/merge-queue/promote with a
// fixed status + body and records each request's path and body.
type mqPromoteFake struct {
	mu     sync.Mutex
	status int
	body   string
	paths  []string
	bodies []string
}

func newMQPromoteFake(t *testing.T, fb *mqPromoteFake) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/merge-queue/promote", func(w http.ResponseWriter, r *http.Request) {
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

func (fb *mqPromoteFake) calls() ([]string, []string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]string(nil), fb.paths...), append([]string(nil), fb.bodies...)
}

// TestPromoteMergeQueueEntry_ValidatesAndPassesThrough: a bad UUID and an
// empty reason are refused BEFORE any HTTP call; a valid call posts
// {"reason"} to the run's promote route, passes every wire field through,
// and names fishhawk_get_merge_queue on the entry's repo and base as its
// next_action.
func TestPromoteMergeQueueEntry_ValidatesAndPassesThrough(t *testing.T) {
	fb := &mqPromoteFake{body: mqPromoteWireBody}
	r := newMergeRunResolver(newMQPromoteFake(t, fb))
	ctx := context.Background()

	for _, in := range []promoteMergeQueueEntryInput{
		{RunID: "not-a-uuid", Reason: "x"},
		{RunID: mqPromoteRunID, Reason: ""},
		{RunID: mqPromoteRunID, Reason: " \t "},
	} {
		if _, _, err := r.promoteMergeQueueEntry(ctx, nil, in); err == nil {
			t.Errorf("input %+v: err = nil, want a local refusal", in)
		}
	}
	if paths, _ := fb.calls(); len(paths) != 0 {
		t.Fatalf("backend called %d times on locally refused input, want 0", len(paths))
	}

	_, out, err := r.promoteMergeQueueEntry(ctx, nil, promoteMergeQueueEntryInput{RunID: mqPromoteRunID, Reason: "  unblocks the line  "})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	paths, bodies := fb.calls()
	if len(paths) != 1 || paths[0] != "/v0/runs/"+mqPromoteRunID+"/merge-queue/promote" || bodies[0] != `{"reason":"unblocks the line"}` {
		t.Fatalf("requests = %v %v, want one POST to the run's promote route with the trimmed reason", paths, bodies)
	}
	res := out.Result
	if res.RunID != mqPromoteRunID || res.EntryID != "eeeeeeee-0000-0000-0000-000000000004" || res.Repo != "x/y" ||
		res.BaseRef != "main" || !res.Promoted || res.OldPosition != 3 || res.NewPosition != 1 ||
		res.LiveRunID != "aaaaaaaa-0000-0000-0000-000000000001" || res.AuditSequence == nil || *res.AuditSequence != 42 {
		t.Fatalf("result = %+v, want every wire field passed through", res)
	}
	if !strings.Contains(out.Message, "position 3 to 1") || !strings.Contains(out.Message, "42") {
		t.Errorf("message = %q, want the positions and the audit sequence", out.Message)
	}
	na := out.NextAction
	if na == nil || na.Action != "fishhawk_get_merge_queue" || na.Params["repo"] != "x/y" || na.Params["base_ref"] != "main" {
		t.Fatalf("next_action = %+v, want fishhawk_get_merge_queue on x/y main", na)
	}
}

// TestPromoteMergeQueueEntry_NoopMessage: promoted:false says nothing was
// recorded and still names the read.
func TestPromoteMergeQueueEntry_NoopMessage(t *testing.T) {
	fb := &mqPromoteFake{body: `{"run_id":"` + mqPromoteRunID + `","entry_id":"e","repo":"x/y","base_ref":"main","promoted":false,"old_position":1,"new_position":1}`}
	r := newMergeRunResolver(newMQPromoteFake(t, fb))
	_, out, err := r.promoteMergeQueueEntry(context.Background(), nil, promoteMergeQueueEntryInput{RunID: mqPromoteRunID, Reason: "x"})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if out.Result.Promoted || out.Result.AuditSequence != nil || !strings.Contains(out.Message, "nothing was changed or recorded") {
		t.Fatalf("no-op output = %+v, want promoted:false and a nothing-recorded message", out)
	}
	if out.NextAction == nil || out.NextAction.Action != "fishhawk_get_merge_queue" {
		t.Fatalf("next_action = %+v, want fishhawk_get_merge_queue", out.NextAction)
	}
}

// TestPromoteMergeQueueEntry_RefusalIsToolErrorWithHint: each backend refusal
// is a TOOL ERROR naming the code, with the remedy hint where one exists.
func TestPromoteMergeQueueEntry_RefusalIsToolErrorWithHint(t *testing.T) {
	for _, tc := range []struct {
		status    int
		code      string
		wantInErr string
	}{
		{http.StatusConflict, "merge_queue_entry_live", "never preempted"},
		{http.StatusConflict, "merge_queue_not_queued", "fishhawk_promote_queued_stage"},
		{http.StatusConflict, "merge_queue_entry_yielded", "next_step"},
		{http.StatusForbidden, "run_token_forbidden", "operator session"},
		{http.StatusForbidden, "insufficient_scope", "write:runs"},
		{http.StatusInternalServerError, "merge_queue_promote_failed", "merge_queue_promote_failed"},
		{http.StatusServiceUnavailable, "merge_queue_unconfigured", "merge_queue_unconfigured"},
	} {
		fb := &mqPromoteFake{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
		r := newMergeRunResolver(newMQPromoteFake(t, fb))
		_, out, err := r.promoteMergeQueueEntry(context.Background(), nil, promoteMergeQueueEntryInput{RunID: mqPromoteRunID, Reason: "x"})
		if err == nil || !strings.Contains(err.Error(), tc.code) || !strings.Contains(err.Error(), tc.wantInErr) {
			t.Errorf("%s: err = %v, want a tool error naming %s and %q", tc.code, err, tc.code, tc.wantInErr)
		}
		if out.NextAction != nil || out.Result.Promoted {
			t.Errorf("%s: output %+v on a refusal, want empty", tc.code, out)
		}
	}
}

// TestPromoteMergeQueueEntry_RegisteredDescription: the registered tool's
// description is distinct from fishhawk_promote_queued_stage and points a
// concurrency waiter at that tool; the SDK validates the structured output
// over a real session.
func TestPromoteMergeQueueEntry_RegisteredDescription(t *testing.T) {
	fb := &mqPromoteFake{body: mqPromoteWireBody}
	srv := newMQPromoteFake(t, fb)
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerPromoteMergeQueueEntry(server, newMergeRunResolver(srv))
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
	for _, want := range []string{"fishhawk_promote_queued_stage", "never preempted", "merge_queue_promoted", "fishhawk_get_merge_queue", "write:runs"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description missing %q", want)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_promote_merge_queue_entry",
		Arguments: map[string]any{"run_id": mqPromoteRunID, "reason": "x"}})
	if err != nil || res.IsError {
		t.Fatalf("CallTool = %+v, %v; want a structured result", res, err)
	}
}
