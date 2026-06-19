package httpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeBackend builds a httptest.Server with handlers shaped like
// the production endpoints. Tests can drive each one's behavior via
// fields on fakeBackend.
type fakeBackend struct {
	startStatus  int
	startResp    Run
	getStatus    int
	getResp      Run
	listResp     ListRunsResult
	listStatus   int
	cancelStatus int
	cancelResp   Run
	errBody      string

	gotAuthHeader string
	gotQuery      string
	gotStartBody  []byte
}

func newFakeBackend(t *testing.T) (*fakeBackend, *httptest.Server) {
	t.Helper()
	fb := &fakeBackend{
		startStatus:  http.StatusCreated,
		getStatus:    http.StatusOK,
		listStatus:   http.StatusOK,
		cancelStatus: http.StatusOK,
	}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	writeErr := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}

	mux.HandleFunc("POST /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		fb.gotAuthHeader = r.Header.Get("Authorization")
		fb.gotStartBody, _ = io.ReadAll(r.Body)
		if fb.startStatus >= 400 {
			writeErr(w, fb.startStatus, fb.errBody)
			return
		}
		writeJSON(w, fb.startStatus, fb.startResp)
	})
	mux.HandleFunc("GET /v0/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		if fb.getStatus >= 400 {
			writeErr(w, fb.getStatus, fb.errBody)
			return
		}
		writeJSON(w, fb.getStatus, fb.getResp)
	})
	mux.HandleFunc("GET /v0/runs", func(w http.ResponseWriter, r *http.Request) {
		fb.gotQuery = r.URL.RawQuery
		if fb.listStatus >= 400 {
			writeErr(w, fb.listStatus, fb.errBody)
			return
		}
		writeJSON(w, fb.listStatus, fb.listResp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if fb.cancelStatus >= 400 {
			writeErr(w, fb.cancelStatus, fb.errBody)
			return
		}
		writeJSON(w, fb.cancelStatus, fb.cancelResp)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func TestStartRun_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.startResp = Run{
		ID: id, Repo: "x/y", WorkflowID: "w",
		WorkflowSHA: "abc", TriggerSource: "cli", State: "pending",
		CreatedAt: time.Now().UTC(),
	}
	c := New(srv.URL, "tok-123")
	got, err := c.StartRun(context.Background(), CreateRunInput{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "abc", TriggerSource: "cli",
	})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID round-trip: %s vs %s", got.ID, id)
	}
	if fb.gotAuthHeader != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", fb.gotAuthHeader)
	}
}

// TestStartRun_BudgetOverride_Serializes asserts the --override-budget
// flag's value reaches the wire body as budget_override=true (#688),
// and that the field is omitted when false (omitempty).
func TestStartRun_BudgetOverride_Serializes(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startResp = Run{ID: uuid.New(), State: "pending"}
	c := New(srv.URL, "")

	if _, err := c.StartRun(context.Background(), CreateRunInput{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "abc", TriggerSource: "cli",
		BudgetOverride: true,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fb.gotStartBody), `"budget_override":true`) {
		t.Errorf("body missing budget_override:true: %s", fb.gotStartBody)
	}

	// Default (false) → omitted from the wire body.
	if _, err := c.StartRun(context.Background(), CreateRunInput{
		Repo: "x/y", WorkflowID: "w", WorkflowSHA: "abc", TriggerSource: "cli",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fb.gotStartBody), "budget_override") {
		t.Errorf("budget_override present when false: %s", fb.gotStartBody)
	}
}

func TestStartRun_NoTokenOmitsAuthHeader(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startResp = Run{ID: uuid.New(), State: "pending"}
	c := New(srv.URL, "") // no token
	if _, err := c.StartRun(context.Background(), CreateRunInput{Repo: "x/y", WorkflowID: "w", WorkflowSHA: "abc", TriggerSource: "cli"}); err != nil {
		t.Fatal(err)
	}
	if fb.gotAuthHeader != "" {
		t.Errorf("Authorization sent when token empty: %q", fb.gotAuthHeader)
	}
}

func TestStartRun_APIError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.startStatus = http.StatusBadRequest
	fb.errBody = `{"error":{"code":"validation_failed","message":"repo is required","details":{"field":"repo"}}}`
	c := New(srv.URL, "")
	_, err := c.StartRun(context.Background(), CreateRunInput{})
	if err == nil {
		t.Fatal("expected APIError")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	if apiErr.Code != "validation_failed" {
		t.Errorf("Code = %q", apiErr.Code)
	}
	if got, _ := apiErr.Details["field"].(string); got != "repo" {
		t.Errorf("Details.field = %v, want repo", apiErr.Details["field"])
	}
}

func TestStartRun_NonEnvelopedError(t *testing.T) {
	// A 500 with unparseable body should still produce APIError —
	// just without code/message populated. Status code is the only
	// reliable signal.
	fb, srv := newFakeBackend(t)
	fb.startStatus = http.StatusInternalServerError
	fb.errBody = "not json at all"
	c := New(srv.URL, "")
	_, err := c.StartRun(context.Background(), CreateRunInput{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
}

func TestGetRun_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.getResp = Run{ID: id, State: "running"}
	c := New(srv.URL, "")
	got, err := c.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.State != "running" {
		t.Errorf("got %+v", got)
	}
}

func TestGetRun_NotFound(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.getStatus = http.StatusNotFound
	fb.errBody = `{"error":{"code":"run_not_found","message":"no run with that id"}}`
	c := New(srv.URL, "")
	_, err := c.GetRun(context.Background(), uuid.New())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "run_not_found" {
		t.Errorf("err = %v, want APIError run_not_found", err)
	}
}

func TestListRuns_QueryStringEncoding(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.listResp = ListRunsResult{Items: []Run{{ID: uuid.New()}}}
	c := New(srv.URL, "")
	_, err := c.ListRuns(context.Background(), ListRunsFilter{
		Repo: "x/y", WorkflowID: "w", State: "running", Limit: 25, Cursor: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repo=x%2Fy", "workflow_id=w", "state=running", "limit=25", "cursor=abc"} {
		if !strings.Contains(fb.gotQuery, want) {
			t.Errorf("query %q missing %q", fb.gotQuery, want)
		}
	}
}

func TestListRuns_EmptyFiltersDropped(t *testing.T) {
	fb, srv := newFakeBackend(t)
	c := New(srv.URL, "")
	_, _ = c.ListRuns(context.Background(), ListRunsFilter{})
	if fb.gotQuery != "" {
		t.Errorf("query = %q, want empty when no filters", fb.gotQuery)
	}
}

func TestCancelRun_HappyPath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.cancelResp = Run{ID: id, State: "cancelled"}
	c := New(srv.URL, "")
	got, err := c.CancelRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "cancelled" {
		t.Errorf("State = %q", got.State)
	}
}

func TestDo_BadBaseURL(t *testing.T) {
	c := &Client{HTTP: &http.Client{Timeout: time.Second}}
	if _, err := c.GetRun(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected error for empty BaseURL")
	}
}

func TestDo_NetworkError(t *testing.T) {
	c := New("http://127.0.0.1:1", "") // port 1 = "won't connect"
	c.HTTP.Timeout = 100 * time.Millisecond
	_, err := c.GetRun(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected network error")
	}
}

func TestShipLocalPullRequest_201(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	prID := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if !strings.Contains(r.URL.Path, runID.String()) {
			t.Errorf("path %q does not contain run_id %s", r.URL.Path, runID)
		}
		if got := r.URL.Query().Get("stage_id"); got != stageID.String() {
			t.Errorf("stage_id = %q, want %s", got, stageID)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		if sig := r.Header.Get("X-Fishhawk-Signature"); sig != "" {
			t.Errorf("X-Fishhawk-Signature present: %q", sig)
		}
		var in ShipLocalPullRequestInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if in.PRNumber != 42 || in.PRURL != "https://github.com/x/y/pull/42" {
			t.Errorf("body round-trip: %+v", in)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ShipLocalPullRequestResult{
			ID: prID, StageID: stageID, ContentHash: "abc123",
			PRNumber: 42, PRURL: "https://github.com/x/y/pull/42",
			HeadSHA: "deadbeef", Idempotent: false,
		})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "test-token")
	got, err := c.ShipLocalPullRequest(context.Background(), runID, stageID, ShipLocalPullRequestInput{
		PRNumber: 42, PRURL: "https://github.com/x/y/pull/42",
		Branch: "feat/x", HeadSHA: "deadbeef", BaseSHA: "cafebabe",
		Title: "Add feature", Body: "desc", FilesChangedCount: 3,
	})
	if err != nil {
		t.Fatalf("ShipLocalPullRequest: %v", err)
	}
	if got.ID != prID {
		t.Errorf("ID = %s, want %s", got.ID, prID)
	}
	if got.Idempotent {
		t.Errorf("Idempotent = true, want false for 201")
	}
}

func TestShipLocalPullRequest_Idempotent(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ShipLocalPullRequestResult{
			ID: uuid.New(), StageID: stageID,
			PRNumber: 7, PRURL: "https://github.com/x/y/pull/7",
			HeadSHA: "aabbcc", Idempotent: true,
		})
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "test-token")
	got, err := c.ShipLocalPullRequest(context.Background(), runID, stageID, ShipLocalPullRequestInput{
		PRNumber: 7, PRURL: "https://github.com/x/y/pull/7",
		Branch: "feat/y", HeadSHA: "aabbcc", BaseSHA: "001122",
		Title: "Redo feature", Body: "",
	})
	if err != nil {
		t.Fatalf("ShipLocalPullRequest idempotent: %v", err)
	}
	if !got.Idempotent {
		t.Errorf("Idempotent = false, want true for 200 idempotent response")
	}
}

func TestShipLocalPullRequest_APIError(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"signature_or_bearer_required","message":"provide a bearer token or a valid HMAC signature","details":null}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "")
	_, err := c.ShipLocalPullRequest(context.Background(), runID, stageID, ShipLocalPullRequestInput{
		PRNumber: 1, PRURL: "https://github.com/x/y/pull/1",
		Branch: "b", HeadSHA: "h", BaseSHA: "s", Title: "t", Body: "b",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.Code != "signature_or_bearer_required" {
		t.Errorf("Code = %q, want signature_or_bearer_required", apiErr.Code)
	}
}

func TestGetStage(t *testing.T) {
	stageID := uuid.New()
	runID := uuid.New()
	tests := []struct {
		name        string
		status      int
		body        string
		wantState   string
		wantErrCode string
	}{
		{
			name:   "200 decodes stage",
			status: http.StatusOK,
			body: `{"id":"` + stageID.String() + `","run_id":"` + runID.String() + `",` +
				`"sequence":1,"type":"plan","executor":{"kind":"local","ref":"v1"},` +
				`"state":"awaiting_approval","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}`,
			wantState: "awaiting_approval",
		},
		{
			name:        "404 returns APIError",
			status:      http.StatusNotFound,
			body:        `{"error":{"code":"stage_not_found","message":"no stage with that id"}}`,
			wantErrCode: "stage_not_found",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			c := New(srv.URL, "")
			got, err := c.GetStage(context.Background(), stageID)
			if tc.wantErrCode != "" {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Code != tc.wantErrCode {
					t.Errorf("err = %v, want APIError %q", err, tc.wantErrCode)
				}
				if apiErr.StatusCode != tc.status {
					t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetStage: %v", err)
			}
			if got.ID != stageID {
				t.Errorf("ID = %s, want %s", got.ID, stageID)
			}
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
		})
	}
}

// TestSubmitApproval_DecodesBothResponseShapes pins the #986 wire seam
// from the client side: a bare-Stage 200 (first submission) decodes to
// zero-valued duplicate fields, and a duplicate-labeled 200 surfaces
// duplicate_submission/prior_decision/prior_submitted_at.
func TestSubmitApproval_DecodesBothResponseShapes(t *testing.T) {
	stageID := uuid.New()
	runID := uuid.New()
	stageJSON := `"id":"` + stageID.String() + `","run_id":"` + runID.String() + `",` +
		`"sequence":1,"type":"plan","executor":{"kind":"agent","ref":"claude-code"},` +
		`"state":"succeeded","created_at":"2026-06-10T00:00:00Z","updated_at":"2026-06-10T00:00:00Z"`
	tests := []struct {
		name          string
		body          string
		wantDuplicate bool
		wantPrior     string
	}{
		{
			name: "first submission: bare Stage, zero duplicate fields",
			body: `{` + stageJSON + `}`,
		},
		{
			name: "duplicate: labeled fields decoded",
			body: `{` + stageJSON + `,"duplicate_submission":true,` +
				`"prior_decision":"approve","prior_submitted_at":"2026-06-10T12:00:00Z"}`,
			wantDuplicate: true,
			wantPrior:     "approve",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			c := New(srv.URL, "")
			got, err := c.SubmitApproval(context.Background(), stageID, SubmitApprovalInput{
				Decision: ApprovalApprove,
			})
			if err != nil {
				t.Fatalf("SubmitApproval: %v", err)
			}
			if got.ID != stageID || got.State != "succeeded" {
				t.Errorf("stage = (%s, %s), want (%s, succeeded)", got.ID, got.State, stageID)
			}
			if got.DuplicateSubmission != tc.wantDuplicate {
				t.Errorf("DuplicateSubmission = %v, want %v", got.DuplicateSubmission, tc.wantDuplicate)
			}
			if got.PriorDecision != tc.wantPrior {
				t.Errorf("PriorDecision = %q, want %q", got.PriorDecision, tc.wantPrior)
			}
			if tc.wantDuplicate && got.PriorSubmittedAt == "" {
				t.Errorf("PriorSubmittedAt empty on the duplicate path")
			}
		})
	}
}

// TestSubmitRevise_RequestShaping pins the wire shape of the #1099
// revise client method: the constraint + force flag ride the JSON body,
// the request lands on POST /v0/stages/{id}/revise, and the re-opened
// Stage decodes back.
func TestSubmitRevise_RequestShaping(t *testing.T) {
	stageID := uuid.New()
	runID := uuid.New()

	var gotMethod, gotPath string
	var gotBody struct {
		Constraint          string `json:"constraint"`
		ForceAdditionalPass bool   `json:"force_additional_pass"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"`+stageID.String()+`","run_id":"`+runID.String()+`",`+
			`"sequence":1,"type":"plan","executor":{"kind":"agent","ref":"claude-code"},`+
			`"state":"pending","created_at":"2026-06-15T00:00:00Z","updated_at":"2026-06-15T00:00:00Z"}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "")
	got, err := c.SubmitRevise(context.Background(), stageID, SubmitReviseInput{
		Constraint:          "use the existing retry helper",
		ForceAdditionalPass: true,
	})
	if err != nil {
		t.Fatalf("SubmitRevise: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/v0/stages/" + stageID.String() + "/revise"; gotPath != want {
		t.Errorf("path = %s, want %s", gotPath, want)
	}
	if gotBody.Constraint != "use the existing retry helper" {
		t.Errorf("body constraint = %q, want the threaded constraint", gotBody.Constraint)
	}
	if !gotBody.ForceAdditionalPass {
		t.Errorf("body force_additional_pass = false, want true")
	}
	if got.ID != stageID || got.State != "pending" {
		t.Errorf("stage = (%s, %s), want (%s, pending)", got.ID, got.State, stageID)
	}
}

func TestAPIError_Error(t *testing.T) {
	tests := []struct {
		name string
		err  *APIError
		want string
	}{
		{"with code", &APIError{StatusCode: 400, Code: "x", Message: "y"}, "fishhawk: HTTP 400 (x): y"},
		{"no code", &APIError{StatusCode: 500}, "fishhawk: HTTP 500"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- scope amendments (#1233 interim auto-decider) ---

func TestListScopeAmendments_WaitForwardedAndEnvelopeDecoded(t *testing.T) {
	runID := uuid.New()
	amendID := uuid.New()
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(scopeAmendmentListResult{Items: []ScopeAmendment{
			{
				ID: amendID, RunID: runID, StageID: uuid.New(), Status: "pending",
				Paths:  []ScopeAmendmentPath{{Path: "a/b_test.go", Operation: "create"}},
				Reason: "coupled test",
			},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL, "op-tok")
	items, err := c.ListScopeAmendments(context.Background(), runID, 25)
	if err != nil {
		t.Fatalf("ListScopeAmendments: %v", err)
	}
	if !strings.Contains(gotQuery, "wait=25") {
		t.Errorf("query missing wait=25: %q", gotQuery)
	}
	if len(items) != 1 || items[0].ID != amendID {
		t.Fatalf("decode mismatch: %+v", items)
	}
	if items[0].Paths[0].Path != "a/b_test.go" || items[0].Paths[0].Operation != "create" {
		t.Errorf("path entry decode mismatch: %+v", items[0].Paths)
	}
}

func TestListScopeAmendments_WaitOmittedWhenNonPositive(t *testing.T) {
	var gotQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/scope-amendments", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(scopeAmendmentListResult{Items: nil})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL, "")
	if _, err := c.ListScopeAmendments(context.Background(), uuid.New(), 0); err != nil {
		t.Fatalf("ListScopeAmendments: %v", err)
	}
	if strings.Contains(gotQuery, "wait") {
		t.Errorf("wait should be omitted for <=0: %q", gotQuery)
	}
}

func TestDecideScopeAmendment_RequestBodyShape(t *testing.T) {
	runID := uuid.New()
	amendID := uuid.New()
	var gotBody []byte
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision", func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ScopeAmendment{ID: amendID, RunID: runID, Status: "approved"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL, "op-tok")
	got, err := c.DecideScopeAmendment(context.Background(), runID, amendID, "approve", "because")
	if err != nil {
		t.Fatalf("DecideScopeAmendment: %v", err)
	}
	if !strings.Contains(string(gotBody), `"decision":"approve"`) || !strings.Contains(string(gotBody), `"reason":"because"`) {
		t.Errorf("body shape mismatch: %s", gotBody)
	}
	if got.Status != "approved" {
		t.Errorf("status = %q, want approved", got.Status)
	}
}

func TestDecideScopeAmendment_AlreadyDecidedMapsToAPIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/runs/{run_id}/scope-amendments/{amendment_id}/decision", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":{"code":"amendment_already_decided","message":"already decided"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL, "op-tok")
	_, err := c.DecideScopeAmendment(context.Background(), uuid.New(), uuid.New(), "approve", "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusConflict || apiErr.Code != "amendment_already_decided" {
		t.Errorf("APIError = %+v, want 409 amendment_already_decided", apiErr)
	}
}

func TestListStageArtifacts_EnvelopeDecoded(t *testing.T) {
	stageID := uuid.New()
	schema := "standard_v1"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(listArtifactsResult{Items: []Artifact{
			{ID: uuid.New(), StageID: stageID, Kind: "plan", SchemaVersion: &schema,
				Content: json.RawMessage(`{"scope":{"files":[{"path":"a/b.go"}]}}`)},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL, "op-tok")
	arts, err := c.ListStageArtifacts(context.Background(), stageID)
	if err != nil {
		t.Fatalf("ListStageArtifacts: %v", err)
	}
	if len(arts) != 1 || arts[0].Kind != "plan" || arts[0].SchemaVersion == nil || *arts[0].SchemaVersion != "standard_v1" {
		t.Fatalf("decode mismatch: %+v", arts)
	}
	if !strings.Contains(string(arts[0].Content), "a/b.go") {
		t.Errorf("content decode mismatch: %s", arts[0].Content)
	}
}
