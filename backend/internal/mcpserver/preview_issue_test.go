package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- fishhawk_preview_issue (#3774) ---

// previewIssueFakeBackend serves only POST /v0/work-items/preview. raw keeps
// the last request body as a key map, so a test can assert a key is ABSENT on
// the wire (run_id) rather than merely empty after a typed decode.
type previewIssueFakeBackend struct {
	mu      sync.Mutex
	calls   int
	path    string
	raw     map[string]json.RawMessage
	status  int
	errBody string
}

func newPreviewIssueFakeBackend(t *testing.T) (*previewIssueFakeBackend, *httptest.Server) {
	t.Helper()
	fb := &previewIssueFakeBackend{status: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/work-items/preview", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		raw := map[string]json.RawMessage{}
		_ = json.Unmarshal(b, &raw)
		fb.mu.Lock()
		fb.calls++
		fb.path = r.URL.Path
		fb.raw = raw
		status, errBody := fb.status, fb.errBody
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		_, _ = w.Write([]byte(`{"type":"chore","title":"[E22.5] Add the widget endpoint","body":"b","labels":["type:chore"],
"provider":"github_projects","intake":{"duplicates":[{"number":1240,"title":"Widget endpoint pagination","score":0.5,"confidence":"medium","basis":"widget endpoint","closed":false}],
"derives_from":[{"number":1234,"title":"[E22.4] Add the widget endpoint","url":"https://example.test/1234","closed":false,"in_window":true}],
"score":{"value":0,"unscored":true},"degraded":false,"scanned_items":3,"window_truncated":false,"duration_ms":4}}`))
	})
	// The filing route is served too, and FAILS the test when dialed: a
	// preview that ever reached the filing endpoint would create an issue.
	mux.HandleFunc("POST /v0/work-items", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("fishhawk_preview_issue dialed POST /v0/work-items; a preview must never file")
		w.WriteHeader(http.StatusTeapot)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func (fb *previewIssueFakeBackend) snapshot() (int, string, map[string]json.RawMessage) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.calls, fb.path, fb.raw
}

// TestPreviewIssue_PostsPreviewWithSourceRefsAndNoRunID: the tool POSTs the
// preview route, forwards source_refs and relations, and sends NO run_id key
// even when FISHHAWK_RUN_ID is set (a preview is not run-scoped; the backend
// refuses a run_id 400).
func TestPreviewIssue_PostsPreviewWithSourceRefsAndNoRunID(t *testing.T) {
	fb, srv := newPreviewIssueFakeBackend(t)
	r := newResolver(srv, map[string]string{
		"GITHUB_REPOSITORY": "kuhlman-labs/fishhawk",
		"FISHHAWK_RUN_ID":   "11111111-1111-1111-1111-111111111111",
	})

	_, out, err := r.previewIssue(context.Background(), nil, PreviewIssueInput{
		Type:       "chore",
		Summary:    "Add the widget endpoint",
		SourceRefs: []string{"#1234", "1235"},
		Relations:  &FileIssueRelations{ParentEpic: "#22"},
	})
	if err != nil {
		t.Fatalf("previewIssue: %v", err)
	}
	calls, path, raw := fb.snapshot()
	if calls != 1 || path != "/v0/work-items/preview" {
		t.Fatalf("calls=%d path=%q, want one POST /v0/work-items/preview", calls, path)
	}
	if _, ok := raw["run_id"]; ok {
		t.Errorf("the preview request carries run_id=%s; a preview must never send one", raw["run_id"])
	}
	var refs []string
	if err := json.Unmarshal(raw["source_refs"], &refs); err != nil || len(refs) != 2 || refs[0] != "#1234" || refs[1] != "1235" {
		t.Errorf("source_refs on the wire = %s (err=%v), want [\"#1234\",\"1235\"]", raw["source_refs"], err)
	}
	if string(raw["repo"]) != `"kuhlman-labs/fishhawk"` {
		t.Errorf("repo on the wire = %s, want the GITHUB_REPOSITORY fallback", raw["repo"])
	}
	if !strings.Contains(string(raw["relations"]), `"parent_epic":"#22"`) {
		t.Errorf("relations on the wire = %s, want parent_epic forwarded", raw["relations"])
	}

	pv := out.Preview
	if pv.Title != "[E22.5] Add the widget endpoint" || pv.Provider != "github_projects" {
		t.Errorf("preview = %+v", pv)
	}
	if len(pv.Intake.Duplicates) != 1 || pv.Intake.Duplicates[0].Number != 1240 {
		t.Errorf("intake.duplicates = %+v, want #1240", pv.Intake.Duplicates)
	}
	want := IntakeSourceItem{Number: 1234, Title: "[E22.4] Add the widget endpoint", URL: "https://example.test/1234", InWindow: true}
	if len(pv.Intake.DerivesFrom) != 1 || pv.Intake.DerivesFrom[0] != want {
		t.Errorf("intake.derives_from = %+v, want [%+v]", pv.Intake.DerivesFrom, want)
	}
}

// TestPreviewWorkItemClient_StripsRunID isolates the client-side strip: a
// caller that hands PreviewWorkItem a RunID still sends no run_id key. The
// tool never sets one, so only a DIRECT client call can show the strip is the
// thing keeping it off the wire.
func TestPreviewWorkItemClient_StripsRunID(t *testing.T) {
	fb, srv := newPreviewIssueFakeBackend(t)
	c := newAPIClient(config{backendURL: srv.URL, apiToken: "tok-test"})

	if _, err := c.PreviewWorkItem(context.Background(), FileWorkItemRequest{
		Repo: "o/n", Type: "chore", Summary: "x",
		RunID: "11111111-1111-1111-1111-111111111111",
	}); err != nil {
		t.Fatalf("PreviewWorkItem: %v", err)
	}
	_, _, raw := fb.snapshot()
	if v, ok := raw["run_id"]; ok {
		t.Errorf("PreviewWorkItem sent run_id=%s; the preview route refuses a run_id and the client must never send one", v)
	}
}

// TestPreviewIssue_RunBoundRefusalIsToolError: the backend's 403
// preview_operator_only (a run-bound token) surfaces as a tool error carrying
// the code, so the agent reads WHY rather than a bare status.
func TestPreviewIssue_RunBoundRefusalIsToolError(t *testing.T) {
	fb, srv := newPreviewIssueFakeBackend(t)
	fb.status = http.StatusForbidden
	fb.errBody = `{"error":{"code":"preview_operator_only","message":"previewing a work item is operator-only"}}`
	r := newResolver(srv, nil)

	_, out, err := r.previewIssue(context.Background(), nil, PreviewIssueInput{
		Repo: "o/n", Type: "chore", Summary: "x",
	})
	if err == nil {
		t.Fatalf("want a tool error on a 403, got output %+v", out)
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.StatusCode != http.StatusForbidden || ae.Code != "preview_operator_only" {
		t.Fatalf("err = %v, want a wrapped *apiError 403 preview_operator_only", err)
	}
	if !strings.Contains(err.Error(), "preview work item") {
		t.Errorf("err = %v, want the tool's context prefix", err)
	}
}

// TestPreviewIssue_LocalValidation: missing type / summary / repo fail before
// any HTTP hop.
func TestPreviewIssue_LocalValidation(t *testing.T) {
	cases := map[string]struct {
		in   PreviewIssueInput
		want string
	}{
		"missing type":    {PreviewIssueInput{Repo: "o/n", Summary: "x"}, "type is required"},
		"missing summary": {PreviewIssueInput{Repo: "o/n", Type: "chore"}, "summary is required"},
		"missing repo":    {PreviewIssueInput{Type: "chore", Summary: "x"}, "repo is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fb, srv := newPreviewIssueFakeBackend(t)
			r := newResolver(srv, nil)
			_, _, err := r.previewIssue(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if calls, _, _ := fb.snapshot(); calls != 0 {
				t.Errorf("backend called %d times, want 0 (local fast-fail)", calls)
			}
		})
	}
}

// TestPreviewIssueToolDescription pins the wire-visible description's claims
// an agent acts on: WHEN it applies and WHO is eligible lead, that it creates
// nothing and writes no audit, the run-bound refusal code, source_refs, and
// the advisory/point-in-time posture. Substrings, not sentences, so a
// copy-edit does not silently delete the control.
func TestPreviewIssueToolDescription(t *testing.T) {
	ctx := context.Background()
	cfg := config{backendURL: "http://localhost:8080", apiToken: "tok"}
	srv := buildServer(cfg)
	resolver := &runResolver{api: newAPIClient(cfg), getenv: envFuncFromMap(nil)}
	registerTools(srv, resolver)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer serverSession.Close()
	clientSession, cerr := client.Connect(ctx, clientTransport, nil)
	if cerr != nil {
		t.Fatalf("client connect: %v", cerr)
	}
	defer clientSession.Close()

	res, lerr := clientSession.ListTools(ctx, nil)
	if lerr != nil {
		t.Fatalf("ListTools: %v", lerr)
	}
	var desc string
	var schema []byte
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_preview_issue" {
			desc = tool.Description
			schema, _ = json.Marshal(tool.InputSchema)
		}
	}
	if desc == "" {
		t.Fatal("fishhawk_preview_issue is not registered")
	}
	if !strings.HasPrefix(desc, "Use this when") {
		t.Errorf("description must lead with WHEN:\n%s", desc)
	}
	for _, want := range []string{
		"Eligibility: an operator caller",
		"preview_operator_only",
		"creates NOTHING",
		"no audit",
		"source_refs",
		"derives_from",
		"ADVISORY",
		"reserves nothing",
		"fishhawk_file_issue",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_preview_issue description does not state %q:\n%s", want, desc)
		}
	}
	if strings.Contains(string(schema), `"run_id"`) {
		t.Errorf("the preview input schema exposes run_id; a preview is not run-scoped:\n%s", schema)
	}
	if !strings.Contains(string(schema), `"source_refs"`) {
		t.Errorf("the preview input schema does not expose source_refs:\n%s", schema)
	}
}
