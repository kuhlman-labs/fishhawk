package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- fishhawk_file_issue (#1005) ---

// fileIssueFakeBackend is a self-contained backend stub for the file-issue
// tool: it serves only POST /v0/work-items. lastBody captures the last
// decoded request so tests assert field threading; status drives the HTTP
// status (default 201); errBody, when set, is written verbatim for the
// error-path tests. resp overrides the default echoed item.
type fileIssueFakeBackend struct {
	mu       sync.Mutex
	lastBody FileWorkItemRequest
	calls    int
	status   int
	errBody  string
	resp     *FiledWorkItem
}

func newFileIssueFakeBackend(t *testing.T) (*fileIssueFakeBackend, *httptest.Server) {
	fb := &fileIssueFakeBackend{status: http.StatusCreated}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/work-items", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body FileWorkItemRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		fb.mu.Lock()
		fb.calls++
		fb.lastBody = body
		status := fb.status
		errBody := fb.errBody
		resp := fb.resp
		fb.mu.Unlock()
		w.WriteHeader(status)
		if errBody != "" {
			_, _ = w.Write([]byte(errBody))
			return
		}
		if resp == nil {
			resp = &FiledWorkItem{
				Type:          body.Type,
				Title:         "Add the widget endpoint",
				Number:        1207,
				URL:           "https://github.com/" + body.Repo + "/issues/1207",
				Provider:      "github_projects",
				AppliedLabels: []string{"type:feature"},
				Complexity:    body.Complexity,
				Status:        "Backlog",
				BoardColumn:   "Backlog",
				Audited:       body.RunID != "",
			}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestFileIssue_HappyPath_ThreadsFieldsAndRelations(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo:       "kuhlman-labs/fishhawk",
		Type:       "feature",
		Summary:    "Add the widget endpoint",
		Complexity: "low",
		Labels:     []string{"area:backend"},
		Relations: &FileIssueRelations{
			ParentEpic:   "#1005",
			EvidenceRuns: []string{"run-abc"},
			DependsOn:    []string{"#41", "42"},
		},
	})
	if err != nil {
		t.Fatalf("fileIssue: %v", err)
	}
	if out.Item.Number != 1207 {
		t.Errorf("Number = %d, want 1207", out.Item.Number)
	}
	if out.Item.Provider != "github_projects" {
		t.Errorf("Provider = %q", out.Item.Provider)
	}
	if fb.calls != 1 {
		t.Errorf("backend called %d times, want 1", fb.calls)
	}
	if fb.lastBody.Type != "feature" || fb.lastBody.Summary != "Add the widget endpoint" {
		t.Errorf("body type/summary = %q/%q", fb.lastBody.Type, fb.lastBody.Summary)
	}
	if fb.lastBody.Complexity != "low" {
		t.Errorf("body complexity = %q, want low", fb.lastBody.Complexity)
	}
	if fb.lastBody.Relations == nil || fb.lastBody.Relations.ParentEpic != "#1005" {
		t.Errorf("body relations = %+v", fb.lastBody.Relations)
	}
	if got := fb.lastBody.Relations.DependsOn; len(got) != 2 || got[0] != "#41" || got[1] != "42" {
		t.Errorf("body relations depends_on = %v, want [#41 42]", got)
	}
}

func TestFileIssue_RepoAndRunFromEnv(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, map[string]string{
		"GITHUB_REPOSITORY": "kuhlman-labs/fishhawk",
		"FISHHAWK_RUN_ID":   "11111111-1111-1111-1111-111111111111",
	})

	_, out, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Type:    "bug",
		Summary: "Widget 500s on empty body",
	})
	if err != nil {
		t.Fatalf("fileIssue: %v", err)
	}
	if fb.lastBody.Repo != "kuhlman-labs/fishhawk" {
		t.Errorf("body repo = %q, want env fallback", fb.lastBody.Repo)
	}
	if fb.lastBody.RunID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("body run_id = %q, want env fallback", fb.lastBody.RunID)
	}
	if !out.Item.Audited {
		t.Errorf("Audited = false, want true (run in flight)")
	}
}

func TestFileIssue_MissingType_FailsLocally(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "o/n", Summary: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "type is required") {
		t.Fatalf("err = %v, want type-required error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestFileIssue_MissingSummary_FailsLocally(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "o/n", Type: "feature", Summary: "  ",
	})
	if err == nil || !strings.Contains(err.Error(), "summary is required") {
		t.Fatalf("err = %v, want summary-required error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestFileIssue_MissingRepoNoEnv_FailsLocally(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Type: "feature", Summary: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Fatalf("err = %v, want repo-required error", err)
	}
	if fb.calls != 0 {
		t.Errorf("backend called %d times, want 0", fb.calls)
	}
}

func TestFileIssue_BoardingBestEffort_DecodesThroughMirror(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	fb.resp = &FiledWorkItem{
		Type:          "feature",
		Title:         "Best-effort boarding",
		Number:        1300,
		URL:           "https://github.com/kuhlman-labs/fishhawk/issues/1300",
		Provider:      "github_projects",
		Boarded:       false,
		EpicLinked:    false,
		BoardingError: "workmgmt/github: status \"Backlog\" is not a Status option on the project",
	}
	r := newResolver(srv, nil)

	_, out, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "kuhlman-labs/fishhawk", Type: "feature", Summary: "x",
	})
	if err != nil {
		t.Fatalf("fileIssue: %v", err)
	}
	// The created issue lands; boarded/epic_linked decode through the mirror
	// so the tool renders exactly what landed (#1107).
	if out.Item.Number != 1300 {
		t.Errorf("Number = %d, want 1300", out.Item.Number)
	}
	if out.Item.Boarded {
		t.Errorf("Boarded = true, want false")
	}
	if !strings.Contains(out.Item.BoardingError, "is not a Status option") {
		t.Errorf("BoardingError = %q, want the cause", out.Item.BoardingError)
	}
}

func TestFileIssue_FilingFailed_SurfacesErrorRef(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	fb.status = http.StatusBadGateway
	// Post-#2587 the backend redacts the raw provider cause out of a 5xx body
	// and hands back only error_ref; the operator recovers the cause from the
	// server log keyed by that ref. The fixture is the SHIPPED 5xx shape: no
	// details.error, an error_ref set.
	fb.errBody = `{"error":{"code":"work_item_filing_failed","message":"provider could not file the work item","error_ref":"req-abc123"}}`
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "kuhlman-labs/fishhawk", Type: "feature", Summary: "x",
	})
	if err == nil {
		t.Fatal("want a tool error on a 502")
	}
	// The operator must see the correlation handle plus a pointer to the log.
	if !strings.Contains(err.Error(), "error_ref=req-abc123") {
		t.Errorf("err = %v, want the surfaced error_ref", err)
	}
	if !strings.Contains(err.Error(), "server log") {
		t.Errorf("err = %v, want the where-to-look pointer", err)
	}
}

func TestFileIssue_ProviderUnimplemented_PropagatesError(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	fb.status = http.StatusNotImplemented
	fb.errBody = `{"error":{"code":"provider_unimplemented","message":"work-item provider \"jira\" is not registered"}}`
	r := newResolver(srv, nil)

	_, _, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo: "o/n", Type: "feature", Summary: "x",
	})
	if err == nil || !strings.Contains(err.Error(), "provider_unimplemented") {
		t.Fatalf("err = %v, want provider_unimplemented", err)
	}
}

// TestFileIssue_SurfacesLabelCompleteness proves the tool surfaces the backend's
// #1616 LOUD report through to the tool output: a work-items response carrying
// defaulted_labels + missing_label_namespaces reaches FileIssueOutput.Item so
// the operator sees exactly what filing-time completeness added and what is
// still missing.
func TestFileIssue_SurfacesLabelCompleteness(t *testing.T) {
	fb, srv := newFileIssueFakeBackend(t)
	fb.resp = &FiledWorkItem{
		Type:                   "feature",
		Title:                  "Add the widget endpoint",
		Number:                 1207,
		URL:                    "https://example/1207",
		Provider:               "github_projects",
		AppliedLabels:          []string{"type:feature", "autonomy:medium"},
		DefaultedLabels:        []string{"autonomy:medium"},
		MissingLabelNamespaces: []string{"area"},
	}
	r := newResolver(srv, nil)

	_, out, err := r.fileIssue(context.Background(), nil, FileIssueInput{
		Repo:    "kuhlman-labs/fishhawk",
		Type:    "feature",
		Summary: "Add the widget endpoint",
	})
	if err != nil {
		t.Fatalf("fileIssue: %v", err)
	}
	if strings.Join(out.Item.DefaultedLabels, ",") != "autonomy:medium" {
		t.Errorf("DefaultedLabels = %v, want [autonomy:medium]", out.Item.DefaultedLabels)
	}
	if strings.Join(out.Item.MissingLabelNamespaces, ",") != "area" {
		t.Errorf("MissingLabelNamespaces = %v, want [area]", out.Item.MissingLabelNamespaces)
	}
}

// TestFileIssue_SchemaTitleVarsNamesAutoDerivedN is binding condition (2): the
// advertised file_issue jsonschema — the SAME jsonschema.For inference AddTool
// uses — must name the auto-derived {n} in the title_vars description, so a
// driving agent reads that {n} need not be supplied for a child type (#1958).
func TestFileIssue_SchemaTitleVarsNamesAutoDerivedN(t *testing.T) {
	schema, err := jsonschema.For[FileIssueInput](nil)
	if err != nil {
		t.Fatalf("infer FileIssueInput schema: %v", err)
	}
	tv, ok := schema.Properties["title_vars"]
	if !ok || tv == nil {
		t.Fatalf("title_vars property missing from advertised schema")
	}
	if !strings.Contains(tv.Description, "{n}") {
		t.Errorf("title_vars description must name {n}: %q", tv.Description)
	}
	if !strings.Contains(strings.ToLower(tv.Description), "auto-derived") {
		t.Errorf("title_vars description must state {n} is auto-derived: %q", tv.Description)
	}
}

// TestFileIssueToolDescribesIntakeAsAdvisory is a DONE-MEANS test for the
// shipped tool description (#2239). The description is the agent's only
// instruction about how to treat the new intake object, and its posture —
// advisory, nothing was acted on, a degradation is normal — is a convention no
// compiler enforces. A comment-only or no-op touch of file_issue.go would pass
// a scope-presence check and fail here.
//
// The PAYLOAD boundary itself is pinned where the real bytes are: the server
// package's TestFileWorkItem_IntakeSignalsEndToEnd decodes the handler's actual
// 201 into mcpserver.FiledWorkItem with DisallowUnknownFields, so these local
// decode-only mirrors are checked against ONE source of truth rather than a
// second fixture that could drift (binding approval condition L3).
func TestFileIssueToolDescribesIntakeAsAdvisory(t *testing.T) {
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
	for _, tool := range res.Tools {
		if tool.Name == "fishhawk_file_issue" {
			desc = tool.Description
		}
	}
	if desc == "" {
		t.Fatal("fishhawk_file_issue is not registered")
	}
	for _, want := range []string{
		"intake",
		"ADVISORY",
		"do NOT",
		"degraded:true",
		"degrade_reason",
		"LEXICAL",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("fishhawk_file_issue description does not mention %q; the intake posture is not stated to the agent", want)
		}
	}
	// Every degrade reason the backend can report must be enumerated, so an
	// agent never sees an unexplained value.
	for _, reason := range []string{
		"reader_unavailable", "reader_error", "charter_undeclared", "charter_unresolved",
		"charter_rubric_unparsed", "budget_exceeded", "hook_panic", "seam_unwired",
	} {
		if !strings.Contains(desc, reason) {
			t.Errorf("degrade reason %q is not enumerated in the tool description", reason)
		}
	}
}

// TestFiledWorkItemDecodesIntakeShape pins the LOCAL decode-only mirrors
// (ADR-064): they must decode the backend's field names and nesting. The
// end-to-end drift check lives in the server package against real bytes; this
// is the unit-level shape pin that keeps a rename here visible.
func TestFiledWorkItemDecodesIntakeShape(t *testing.T) {
	const payload = `{"type":"chore","title":"t","number":1,"url":"u","provider":"github_projects",
"intake":{"duplicates":[{"number":7,"url":"x","title":"dup","score":0.7,"confidence":"high","basis":"a b","closed":true}],
"epic_suggestion":{"number":22,"title":"[E22] e","score":0.5,"confidence":"medium","basis":"c"},
"score":{"value":4.0,"citations":[{"rubric_id":"S2","quote":"q","note":"n"}],"unscored":false},
"degraded":false,"scanned_items":3,"window_truncated":true,"duration_ms":12}}`

	var out FiledWorkItem
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Intake == nil {
		t.Fatal("intake dropped")
	}
	if len(out.Intake.Duplicates) != 1 || out.Intake.Duplicates[0].Number != 7 ||
		out.Intake.Duplicates[0].Confidence != "high" || !out.Intake.Duplicates[0].Closed {
		t.Errorf("duplicates = %+v", out.Intake.Duplicates)
	}
	if out.Intake.EpicSuggestion == nil || out.Intake.EpicSuggestion.Number != 22 {
		t.Errorf("epic_suggestion = %+v", out.Intake.EpicSuggestion)
	}
	if len(out.Intake.Score.Citations) != 1 || out.Intake.Score.Citations[0].RubricID != "S2" {
		t.Errorf("citations = %+v", out.Intake.Score.Citations)
	}
	if !out.Intake.WindowTruncated || out.Intake.ScannedItems != 3 || out.Intake.DurationMS != 12 {
		t.Errorf("window/scan/duration = %+v", out.Intake)
	}
}
