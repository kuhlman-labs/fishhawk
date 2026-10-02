package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const schedulesTestRepo = "kuhlman-labs/fishhawk"

// schedulesStubBackend answers GET /v0/schedules with status/body, recording
// each request line.
type schedulesStubBackend struct {
	requests []string
	status   int
	body     string
}

func (b *schedulesStubBackend) serve(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		if b.status != 0 {
			w.WriteHeader(b.status)
		}
		_, _ = io.WriteString(w, b.body)
	}))
	t.Cleanup(ts.Close)
	return ts
}

const schedulesPopulatedBody = `{"repo":"` + schedulesTestRepo + `","enabled":true,"repo_scanned":true,` +
	`"last_tick_at":"2026-10-02T14:30:00Z","runner_kind":"local",` +
	`"dispatch_note":"runner_kind is local: a scheduled run parks at awaiting_host_dispatch until a host dispatches it","schedules":[` +
	`{"workflow_id":"backlog_grooming","cron":"0 9 * * 1","timezone":"America/Chicago","issue":3112,` +
	`"current_window_start":"2026-09-28T14:00:00Z","next_due_at":"2026-10-05T14:00:00Z",` +
	`"last_outcome":{"kind":"refused","window_start":"2026-09-28T14:00:00Z","code":"budget_exhausted","message":"monthly budget exhausted","at":"2026-09-28T14:00:05Z"}},` +
	`{"workflow_id":"upkeep","cron":"*/15 * * * *","timezone":"UTC","current_window_start":null,"next_due_at":null,"last_outcome":null}]}`

const schedulesDisabledBody = `{"repo":"` + schedulesTestRepo + `","enabled":false,"reason":"the scheduler is not enabled on this deployment","repo_scanned":false,"last_tick_at":null,"schedules":[]}`

// TestListSchedulesTool_ReadsAndPassesThrough: one GET /v0/schedules with the
// repo query-escaped, every field carried verbatim, no elisions under budget.
func TestListSchedulesTool_ReadsAndPassesThrough(t *testing.T) {
	b := &schedulesStubBackend{body: schedulesPopulatedBody}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.listSchedules(context.Background(), nil, ListSchedulesInput{Repo: schedulesTestRepo})
	if err != nil {
		t.Fatalf("listSchedules: %v", err)
	}
	if len(b.requests) != 1 || b.requests[0] != "GET /v0/schedules?repo=kuhlman-labs%2Ffishhawk" {
		t.Errorf("requests = %v, want one GET /v0/schedules?repo=kuhlman-labs%%2Ffishhawk", b.requests)
	}
	if !out.Enabled || !out.RepoScanned || out.RunnerKind != "local" || !strings.Contains(out.DispatchNote, "awaiting_host_dispatch") {
		t.Errorf("header = %+v", out)
	}
	if len(out.Schedules) != 2 {
		t.Fatalf("schedules = %d, want 2", len(out.Schedules))
	}
	g := out.Schedules[0]
	if g.WorkflowID != "backlog_grooming" || g.Issue != 3112 || g.CurrentWindowStart == nil || g.NextDueAt == nil {
		t.Errorf("grooming = %+v", g)
	}
	if g.LastOutcome == nil || g.LastOutcome.Kind != "refused" || g.LastOutcome.Code != "budget_exhausted" {
		t.Errorf("last_outcome = %+v, want the refusal carried verbatim", g.LastOutcome)
	}
	if u := out.Schedules[1]; u.CurrentWindowStart != nil || u.NextDueAt != nil || u.LastOutcome != nil {
		t.Errorf("upkeep = %+v, want null window/next/outcome", u)
	}
	if out.Elisions != nil {
		t.Errorf("elisions = %+v, want none under budget", out.Elisions)
	}
}

// TestListSchedulesTool_RepoFallsBackToEnv: an omitted repo resolves from
// GITHUB_REPOSITORY; with neither, the tool refuses BEFORE any request.
func TestListSchedulesTool_RepoFallsBackToEnv(t *testing.T) {
	b := &schedulesStubBackend{body: schedulesDisabledBody}
	r := digestResolver(b.serve(t).URL, map[string]string{"GITHUB_REPOSITORY": schedulesTestRepo})
	if _, _, err := r.listSchedules(context.Background(), nil, ListSchedulesInput{}); err != nil {
		t.Fatalf("listSchedules: %v", err)
	}
	if len(b.requests) != 1 || b.requests[0] != "GET /v0/schedules?repo=kuhlman-labs%2Ffishhawk" {
		t.Errorf("requests = %v", b.requests)
	}

	none := &schedulesStubBackend{body: schedulesDisabledBody}
	r = digestResolver(none.serve(t).URL, nil)
	if _, _, err := r.listSchedules(context.Background(), nil, ListSchedulesInput{Repo: "  "}); err == nil ||
		!strings.Contains(err.Error(), "repo is required") {
		t.Errorf("err = %v, want the repo-required refusal", err)
	}
	if len(none.requests) != 0 {
		t.Errorf("requests = %v, want none (refused locally)", none.requests)
	}
}

// TestListSchedulesTool_BackendRefusalIsToolError: a backend refusal surfaces
// as the typed *apiError with its code — never as an empty schedules list,
// which would read as "nothing is scheduled".
func TestListSchedulesTool_BackendRefusalIsToolError(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "validation_failed"},
		{http.StatusUnauthorized, "authentication_required"},
		{http.StatusForbidden, "repo_forbidden"},
		{http.StatusServiceUnavailable, "service_unavailable"},
	} {
		b := &schedulesStubBackend{status: tc.status, body: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
		r := digestResolver(b.serve(t).URL, nil)
		_, out, err := r.listSchedules(context.Background(), nil, ListSchedulesInput{Repo: schedulesTestRepo})
		var ae *apiError
		if !errors.As(err, &ae) || ae.StatusCode != tc.status || ae.Code != tc.code {
			t.Errorf("err = %v, want *apiError %d %s", err, tc.status, tc.code)
		}
		if out.Schedules != nil || out.Enabled {
			t.Errorf("refusal output = %+v, want the zero value", out)
		}
	}
}

// TestListSchedulesTool_NullSchedulesNormalized: a body whose schedules is null
// (or absent) reaches the caller as an EMPTY array — the output schema types it
// as an array, so a null would fail the SDK's output validation.
func TestListSchedulesTool_NullSchedulesNormalized(t *testing.T) {
	b := &schedulesStubBackend{body: `{"repo":"` + schedulesTestRepo + `","enabled":false,"repo_scanned":false,"last_tick_at":null,"schedules":null}`}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.listSchedules(context.Background(), nil, ListSchedulesInput{Repo: schedulesTestRepo})
	if err != nil {
		t.Fatalf("listSchedules: %v", err)
	}
	if out.Schedules == nil {
		t.Error("schedules = nil, want an empty slice")
	}
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), `"schedules":[]`) {
		t.Errorf("wire = %s, want schedules:[]", raw)
	}
}

// callListSchedulesOverSession registers ONLY fishhawk_list_schedules on a
// real MCP server and calls it from a real client over an in-memory transport,
// so the SDK's output-schema validation runs on the marshalled result.
func callListSchedulesOverSession(t *testing.T, r *runResolver, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	registerListSchedules(server, r)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_list_schedules", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return res
}

// TestListSchedulesTool_WireRoundTrip_RealSession drives the registered tool
// over a real client session against an httptest backend, for a populated
// body (null nested times included) and the enabled:false body. Both must
// pass the SDK's output-schema validation and carry the fields on the wire.
func TestListSchedulesTool_WireRoundTrip_RealSession(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"populated", schedulesPopulatedBody, []string{`"workflow_id":"backlog_grooming"`, `"code":"budget_exhausted"`, `"dispatch_note":`, `"next_due_at":null`}},
		{"disabled", schedulesDisabledBody, []string{`"enabled":false`, `"schedules":[]`, `"last_tick_at":null`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &schedulesStubBackend{body: tc.body}
			res := callListSchedulesOverSession(t, digestResolver(b.serve(t).URL, nil), map[string]any{"repo": schedulesTestRepo})
			if res.IsError {
				t.Fatalf("CallTool IsError; content: %+v", res.Content)
			}
			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("marshal StructuredContent: %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(raw), w) {
					t.Errorf("structured content missing %s: %s", w, raw)
				}
			}
			if len(b.requests) != 1 || b.requests[0] != "GET /v0/schedules?repo=kuhlman-labs%2Ffishhawk" {
				t.Errorf("requests = %v", b.requests)
			}
		})
	}
}

// schedulesManyBody renders n schedules, each carrying a refusal message of
// msgLen bytes, plus a spec_error of specErrLen bytes.
func schedulesManyBody(n, msgLen, specErrLen int) string {
	var items []string
	for i := 0; i < n; i++ {
		items = append(items, fmt.Sprintf(`{"workflow_id":"wf_%03d","cron":"0 9 * * *","timezone":"UTC","current_window_start":"2026-10-02T09:00:00Z","next_due_at":"2026-10-03T09:00:00Z","last_outcome":{"kind":"refused","window_start":"2026-10-02T09:00:00Z","code":"budget_exhausted","message":"%s","at":"2026-10-02T09:00:01Z"}}`,
			i, strings.Repeat("m", msgLen)))
	}
	return `{"repo":"` + schedulesTestRepo + `","enabled":true,"repo_scanned":true,"last_tick_at":"2026-10-02T09:00:00Z","spec_error":"` +
		strings.Repeat("e", specErrLen) + `","runner_kind":"github_actions","schedules":[` + strings.Join(items, ",") + `]}`
}

// TestListSchedulesTool_BoundsEveryTier drives each ladder tier at the 4 KiB
// convergence floor and asserts the result fits, names the tier, and points at
// the unbounded REST read. B1 alone suffices for one oversized message; a list
// too long even after B1 reaches B2, which drops from the TAIL so the
// workflow_id-ascending head survives.
func TestListSchedulesTool_BoundsEveryTier(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	env := map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)}
	const pointer = "GET /v0/schedules?repo=kuhlman-labs%2Ffishhawk"

	t.Run("B1 caps strings", func(t *testing.T) {
		b := &schedulesStubBackend{body: schedulesManyBody(2, 3000, 3000)}
		_, out, err := digestResolver(b.serve(t).URL, env).listSchedules(context.Background(), nil, ListSchedulesInput{Repo: schedulesTestRepo})
		if err != nil {
			t.Fatalf("listSchedules: %v", err)
		}
		raw, _ := json.Marshal(out)
		if len(raw) > budget {
			t.Errorf("output %d bytes, want <= %d", len(raw), budget)
		}
		if out.Elisions == nil || out.Elisions.Tier != "B1" || len(out.Elisions.Fields) != 1 {
			t.Fatalf("elisions = %+v, want one B1 entry", out.Elisions)
		}
		f := out.Elisions.Fields[0]
		if f.OmittedCount != 3 || f.Pointer != pointer || f.Class != "oversized_capable" {
			t.Errorf("B1 field = %+v, want 3 capped strings pointing at %s", f, pointer)
		}
		if len(out.Schedules) != 2 || len(out.SpecError) > schedulesStringCap {
			t.Errorf("B1 dropped schedules or left spec_error uncapped: %d schedules, spec_error %d bytes", len(out.Schedules), len(out.SpecError))
		}
	})

	t.Run("B2 drops from the tail", func(t *testing.T) {
		b := &schedulesStubBackend{body: schedulesManyBody(40, 10, 0)}
		res := callListSchedulesOverSession(t, digestResolver(b.serve(t).URL, env), map[string]any{"repo": schedulesTestRepo})
		if res.IsError {
			t.Fatalf("CallTool IsError (a bounded result must pass output validation); content: %+v", res.Content)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var out ListSchedulesOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(raw) > budget {
			t.Errorf("output %d bytes, want <= %d", len(raw), budget)
		}
		if out.Elisions == nil || out.Elisions.Tier != "B2" {
			t.Fatalf("elisions = %+v, want tier B2", out.Elisions)
		}
		var dropped *ElidedField
		for i := range out.Elisions.Fields {
			if out.Elisions.Fields[i].Field == "schedules" {
				dropped = &out.Elisions.Fields[i]
			}
		}
		if dropped == nil || dropped.OmittedCount != 40-len(out.Schedules) || dropped.Pointer != pointer {
			t.Errorf("schedules elision = %+v, want omitted_count %d pointing at %s", dropped, 40-len(out.Schedules), pointer)
		}
		if len(out.Schedules) == 0 || out.Schedules[0].WorkflowID != "wf_000" {
			t.Errorf("retained = %d (first %v), want the head of the list kept", len(out.Schedules), out.Schedules)
		}
	})
}
