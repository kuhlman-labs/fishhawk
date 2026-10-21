package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The fishhawk_get_merge_queue tool (ADR-093 slice 1 D7, E87.4 / #4295) over a
// fake backend that serves the per-base read's RAW wire bytes, written as
// literals in the server's json-tag spelling (backend/internal/server/
// merge_queue_read.go) — never a re-encoding of the client mirror, so a tag
// drift between the two surfaces as an absent field here.

// mergeQueueWireBody is one base, main: A live (verifying) holds the line,
// B held at position 1 by the rebase path (no intent), C held at position 2
// by merge_run (intent). Median line turn 60s, so B's eta is 60 and C's 120;
// A's PR has moved past its anchor.
const mergeQueueWireBody = `{
  "repo": "x/y",
  "bases": [{
    "base_ref": "main",
    "entries": [
      {"run_id": "aaaaaaaa-0000-0000-0000-000000000001", "entry_id": "eeeeeeee-0000-0000-0000-000000000001", "state": "live", "phase": "verifying",
       "merge_intent": true, "merge_intent_at": "2026-10-21T06:00:00Z", "enqueued_at": "2026-10-21T05:59:00Z", "admitted_at": "2026-10-21T06:00:01Z",
       "anchored_head_sha": "a-anchor", "anchored_base_sha": "base-1"},
      {"run_id": "bbbbbbbb-0000-0000-0000-000000000002", "entry_id": "eeeeeeee-0000-0000-0000-000000000002", "state": "held", "position": 1,
       "merge_intent": false, "enqueued_at": "2026-10-21T06:01:00Z", "anchored_head_sha": "b-head", "eta_seconds": 60},
      {"run_id": "cccccccc-0000-0000-0000-000000000003", "entry_id": "eeeeeeee-0000-0000-0000-000000000003", "state": "held", "position": 2,
       "merge_intent": true, "merge_intent_at": "2026-10-21T06:02:00Z", "enqueued_at": "2026-10-21T06:02:00Z", "anchored_head_sha": "c-head", "eta_seconds": 120}
    ],
    "head_of_line": {"run_id": "aaaaaaaa-0000-0000-0000-000000000001", "entry_id": "eeeeeeee-0000-0000-0000-000000000001", "state": "live", "phase": "verifying",
      "anchored_head_sha": "a-anchor", "current_head_sha": "a-moved"},
    "eta_basis": {"samples": 3, "median_seconds": 60, "note": "eta_seconds = position x the median line-turn time"}
  }]
}`

// mergeQueueFake serves GET /v0/repos/{owner}/{name}/merge-queue with a fixed
// status + body and records each request's path and raw query.
type mergeQueueFake struct {
	mu      sync.Mutex
	status  int
	body    string
	paths   []string
	queries []string
}

func newMergeQueueFake(t *testing.T, fb *mergeQueueFake) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/repos/{owner}/{name}/merge-queue", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.paths = append(fb.paths, r.URL.EscapedPath())
		fb.queries = append(fb.queries, r.URL.RawQuery)
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

func (fb *mergeQueueFake) requests() ([]string, []string) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]string(nil), fb.paths...), append([]string(nil), fb.queries...)
}

// callMergeQueueToolWire drives the REGISTERED fishhawk_get_merge_queue over a
// real MCP client session (so the SDK validates the structured output against
// the reflected schema) and returns it as GENERIC JSON.
func callMergeQueueToolWire(t *testing.T, srv *httptest.Server, args map[string]any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerGetMergeQueue(server, newMergeRunResolver(srv))
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_get_merge_queue", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError || res.StructuredContent == nil {
		return res, nil
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode tool output: %v; raw %s", err, raw)
	}
	return res, generic
}

// TestGetMergeQueue_HeadOfLineAndETA is the operator criterion
// merge-queue-read-head-of-line-and-eta on the MCP surface: the head of line
// carries both the anchored and the current head, held entries carry their
// position and eta_seconds (position x the 60s median), the live entry
// carries neither, merge_intent survives as true / false, and base_ref rides
// the query string. Every key is read as a literal off the tool's wire output.
func TestGetMergeQueue_HeadOfLineAndETA(t *testing.T) {
	fb := &mergeQueueFake{body: mergeQueueWireBody}
	srv := newMergeQueueFake(t, fb)
	_, out := callMergeQueueToolWire(t, srv, map[string]any{"repo": "x/y", "base_ref": "main"})
	if out == nil {
		t.Fatal("tool returned an error or no structured output")
	}
	paths, queries := fb.requests()
	if len(paths) != 1 || paths[0] != "/v0/repos/x/y/merge-queue" || queries[0] != "base_ref=main" {
		t.Fatalf("requests = %v ? %v, want one GET /v0/repos/x/y/merge-queue?base_ref=main", paths, queries)
	}
	if out["repo"] != "x/y" {
		t.Errorf("repo = %v, want x/y", out["repo"])
	}
	bases := wireArr(t, out, "bases", "output")
	if len(bases) != 1 {
		t.Fatalf("bases = %d, want 1", len(bases))
	}
	base := wireObj(t, bases[0], "bases[0]")
	if base["base_ref"] != "main" {
		t.Errorf("base_ref = %v, want main", base["base_ref"])
	}
	hol := wireObj(t, base["head_of_line"], "head_of_line")
	if hol["run_id"] != "aaaaaaaa-0000-0000-0000-000000000001" || hol["state"] != "live" || hol["phase"] != "verifying" ||
		hol["anchored_head_sha"] != "a-anchor" || hol["current_head_sha"] != "a-moved" {
		t.Errorf("head_of_line = %v, want A live/verifying anchored a-anchor, current a-moved", hol)
	}
	if _, present := hol["current_head_unavailable"]; present {
		t.Errorf("head_of_line carries current_head_unavailable on a successful read: %v", hol)
	}
	basis := wireObj(t, base["eta_basis"], "eta_basis")
	if basis["samples"] != float64(3) || basis["median_seconds"] != float64(60) || basis["note"] == "" {
		t.Errorf("eta_basis = %v, want samples 3, median 60, a note", basis)
	}
	entries := wireArr(t, base, "entries", "bases[0]")
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	want := []struct {
		state    string
		position any
		eta      any
		intent   bool
	}{
		{"live", nil, nil, true},
		{"held", float64(1), float64(60), false},
		{"held", float64(2), float64(120), true},
	}
	for i, w := range want {
		e := wireObj(t, entries[i], "entries[i]")
		if e["state"] != w.state || e["position"] != w.position || e["eta_seconds"] != w.eta {
			t.Errorf("entries[%d] = state %v position %v eta %v, want %s/%v/%v", i, e["state"], e["position"], e["eta_seconds"], w.state, w.position, w.eta)
		}
		intent, present := e["merge_intent"]
		if !present || intent != w.intent {
			t.Errorf("entries[%d].merge_intent = %v (present %v), want %v always on the wire", i, intent, present, w.intent)
		}
	}
}

// TestGetMergeQueue_NoBaseRefReadsEveryBase: an omitted base_ref sends no
// query at all, and an empty (or null) bases list stays an empty ARRAY on the
// tool output, which the reflected schema requires.
func TestGetMergeQueue_NoBaseRefReadsEveryBase(t *testing.T) {
	for _, body := range []string{`{"repo":"x/y","bases":[]}`, `{"repo":"x/y","bases":null}`} {
		fb := &mergeQueueFake{body: body}
		srv := newMergeQueueFake(t, fb)
		res, out := callMergeQueueToolWire(t, srv, map[string]any{"repo": "x/y"})
		if out == nil {
			t.Fatalf("body %s: tool error %+v", body, res.Content)
		}
		bases, ok := out["bases"].([]any)
		if !ok || len(bases) != 0 {
			t.Errorf("body %s: bases = %#v, want an empty array", body, out["bases"])
		}
		_, queries := fb.requests()
		if len(queries) != 1 || queries[0] != "" {
			t.Errorf("body %s: queries = %v, want one request with no query", body, queries)
		}
	}
}

// TestGetMergeQueue_MalformedRepoFailsLocally: a repo that is not owner/name
// is refused before the HTTP hop.
func TestGetMergeQueue_MalformedRepoFailsLocally(t *testing.T) {
	fb := &mergeQueueFake{body: mergeQueueWireBody}
	srv := newMergeQueueFake(t, fb)
	r := newMergeRunResolver(srv)
	for _, repo := range []string{"", "x", "/y", "x/", "x/y/z"} {
		if _, _, err := r.getMergeQueue(context.Background(), nil, getMergeQueueInput{Repo: repo}); err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("repo %q: err = %v, want an owner/name refusal", repo, err)
		}
	}
	if paths, _ := fb.requests(); len(paths) != 0 {
		t.Errorf("malformed repos reached the backend: %v", paths)
	}
}

// TestGetMergeQueue_BackendRefusalIsToolError: a backend refusal — the
// run-bound 403, the unwired-queue 503 — is a TOOL ERROR naming the code,
// never an empty queue.
func TestGetMergeQueue_BackendRefusalIsToolError(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusForbidden, "run_token_forbidden"},
		{http.StatusServiceUnavailable, "merge_queue_unconfigured"},
	} {
		fb := &mergeQueueFake{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
		r := newMergeRunResolver(newMergeQueueFake(t, fb))
		_, out, err := r.getMergeQueue(context.Background(), nil, getMergeQueueInput{Repo: "x/y"})
		if err == nil || !strings.Contains(err.Error(), tc.code) {
			t.Errorf("%d: err = %v, want a tool error naming %s", tc.status, err, tc.code)
		}
		if out.Bases != nil {
			t.Errorf("%d: output carries bases %v on a refusal", tc.status, out.Bases)
		}
	}
}
