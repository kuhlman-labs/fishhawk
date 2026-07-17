package gitlabclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"
)

// opRequest is a snapshot of a request the forge-ops stub saw, with the
// body pre-read. Unlike client_test.go's recordedRequest it also captures
// rawQuery, because the branch/compare/list operations carry their
// parameters in the query string and the tests assert them.
type opRequest struct {
	method      string
	path        string
	escapedPath string
	rawQuery    string
	header      http.Header
	body        []byte
}

// opStub is a programmable Doer that records each request and answers via
// handler. It mirrors client_test.go's stubDoer but records rawQuery.
type opStub struct {
	t        *testing.T
	requests []*opRequest
	handler  func(*opRequest) (*http.Response, error)
}

func (s *opStub) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			s.t.Fatalf("read request body: %v", err)
		}
		body = b
	}
	rec := &opRequest{
		method:      req.Method,
		path:        req.URL.Path,
		escapedPath: req.URL.EscapedPath(),
		rawQuery:    req.URL.RawQuery,
		header:      req.Header.Clone(),
		body:        body,
	}
	s.requests = append(s.requests, rec)
	return s.handler(rec)
}

// clientWith builds a Client over an opStub whose handler is fn.
func clientWith(t *testing.T, fn func(*opRequest) (*http.Response, error)) *Client {
	t.Helper()
	return New(testBaseURL, testToken, WithHTTPClient(&opStub{t: t, handler: fn}))
}

// mustQuery parses a recorded rawQuery, failing the test on error.
func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse query %q: %v", raw, err)
	}
	return v
}

// assertAPIError asserts err is an *APIError carrying want as its status.
func assertAPIError(t *testing.T, err error, want int) {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != want {
		t.Errorf("status = %d, want %d", apiErr.StatusCode, want)
	}
}

// --- CreateBranch -----------------------------------------------------------

func TestCreateBranch_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodPost {
			t.Errorf("method = %s, want POST", rec.method)
		}
		if rec.path != "/api/v4/projects/42/repository/branches" {
			t.Errorf("path = %s", rec.path)
		}
		assertPrivateToken(t, rec.header)
		q := mustQuery(t, rec.rawQuery)
		if q.Get("branch") != "feature/x" {
			t.Errorf("branch = %q, want feature/x", q.Get("branch"))
		}
		if q.Get("ref") != "main" {
			t.Errorf("ref = %q, want main", q.Get("ref"))
		}
		if len(rec.body) != 0 {
			t.Errorf("POST carried a body: %s", rec.body)
		}
		return jsonResponse(http.StatusCreated, `{"name":"feature/x","protected":false,"commit":{"id":"abc123"}}`), nil
	})
	br, err := c.CreateBranch(context.Background(), 42, "feature/x", "main")
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if br.Name != "feature/x" || br.Commit == nil || br.Commit.ID != "abc123" {
		t.Errorf("branch = %+v", br)
	}
}

func TestCreateBranch_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"message":"Branch already exists"}`), nil
	})
	_, err := c.CreateBranch(context.Background(), 42, "b", "main")
	assertAPIError(t, err, http.StatusBadRequest)
}

func TestCreateBranch_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.CreateBranch(context.Background(), 0, "b", "main"); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.CreateBranch(context.Background(), 42, " ", "main"); err == nil {
		t.Error("want error for empty branch")
	}
	if _, err := c.CreateBranch(context.Background(), 42, "b", " "); err == nil {
		t.Error("want error for empty ref")
	}
}

// --- GetBranch --------------------------------------------------------------

func TestGetBranch_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		// A slash-bearing branch name is percent-encoded into one segment.
		if want := "/api/v4/projects/42/repository/branches/feature%2Fx"; rec.escapedPath != want {
			t.Errorf("escaped path = %s, want %s", rec.escapedPath, want)
		}
		assertPrivateToken(t, rec.header)
		return jsonResponse(http.StatusOK, `{"name":"feature/x","protected":true,"commit":{"id":"deadbeef"}}`), nil
	})
	br, err := c.GetBranch(context.Background(), 42, "feature/x")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if !br.Protected || br.Commit.ID != "deadbeef" {
		t.Errorf("branch = %+v", br)
	}
}

func TestGetBranch_NotFoundIsAPIError(t *testing.T) {
	// A missing branch surfaces as a 404 *APIError so the adapter can map
	// it to ("", false, nil) via StatusCode.
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404 Branch Not Found"}`), nil
	})
	_, err := c.GetBranch(context.Background(), 42, "gone")
	assertAPIError(t, err, http.StatusNotFound)
}

func TestGetBranch_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.GetBranch(context.Background(), 0, "b"); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.GetBranch(context.Background(), 42, " "); err == nil {
		t.Error("want error for empty branch")
	}
}

// --- DeleteBranch -----------------------------------------------------------

func TestDeleteBranch_RequestShapeAndNoContent(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", rec.method)
		}
		if want := "/api/v4/projects/42/repository/branches/feature%2Fx"; rec.escapedPath != want {
			t.Errorf("escaped path = %s, want %s", rec.escapedPath, want)
		}
		// 204 No Content is success.
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	})
	if err := c.DeleteBranch(context.Background(), 42, "feature/x"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
}

func TestDeleteBranch_NotFoundIsAPIError(t *testing.T) {
	// The delete leg of ForceUpdateRef tolerates a 404 — the adapter reads
	// StatusCode to do so, so the client surfaces it as an *APIError.
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404 Branch Not Found"}`), nil
	})
	assertAPIError(t, c.DeleteBranch(context.Background(), 42, "gone"), http.StatusNotFound)
}

func TestDeleteBranch_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if err := c.DeleteBranch(context.Background(), 0, "b"); err == nil {
		t.Error("want error for missing project id")
	}
	if err := c.DeleteBranch(context.Background(), 42, " "); err == nil {
		t.Error("want error for empty branch")
	}
}

// --- CreateMergeRequest -----------------------------------------------------

func TestCreateMergeRequest_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodPost {
			t.Errorf("method = %s, want POST", rec.method)
		}
		if rec.path != "/api/v4/projects/42/merge_requests" {
			t.Errorf("path = %s", rec.path)
		}
		assertPrivateToken(t, rec.header)
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got["source_branch"] != "run/x" || got["target_branch"] != "main" || got["title"] != "T" {
			t.Errorf("body = %v", got)
		}
		if got["description"] != "D" {
			t.Errorf("description = %v", got["description"])
		}
		return jsonResponse(http.StatusCreated,
			`{"iid":7,"id":900,"project_id":42,"state":"opened","source_branch":"run/x","target_branch":"main","sha":"headsha","web_url":"https://gl/mr/7"}`), nil
	})
	mr, err := c.CreateMergeRequest(context.Background(), 42, CreateMergeRequestParams{
		SourceBranch: "run/x", TargetBranch: "main", Title: "T", Description: "D",
	})
	if err != nil {
		t.Fatalf("CreateMergeRequest: %v", err)
	}
	if mr.IID != 7 || mr.SHA != "headsha" || mr.State != "opened" || mr.WebURL != "https://gl/mr/7" {
		t.Errorf("mr = %+v", mr)
	}
}

func TestCreateMergeRequest_OmitsEmptyDescription(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := got["description"]; ok {
			t.Error("description present though empty")
		}
		return jsonResponse(http.StatusCreated, `{"iid":1}`), nil
	})
	if _, err := c.CreateMergeRequest(context.Background(), 42, CreateMergeRequestParams{
		SourceBranch: "s", TargetBranch: "t", Title: "T",
	}); err != nil {
		t.Fatalf("CreateMergeRequest: %v", err)
	}
}

func TestCreateMergeRequest_Conflict409IsAPIError(t *testing.T) {
	// A 409 (MR already exists) surfaces as an *APIError so the adapter maps
	// it to forge.ErrPullRequestExists.
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusConflict, `{"message":["Another open merge request already exists"]}`), nil
	})
	_, err := c.CreateMergeRequest(context.Background(), 42, CreateMergeRequestParams{
		SourceBranch: "s", TargetBranch: "t", Title: "T",
	})
	assertAPIError(t, err, http.StatusConflict)
}

func TestCreateMergeRequest_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	base := CreateMergeRequestParams{SourceBranch: "s", TargetBranch: "t", Title: "T"}
	if _, err := c.CreateMergeRequest(context.Background(), 0, base); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.CreateMergeRequest(context.Background(), 42, CreateMergeRequestParams{TargetBranch: "t", Title: "T"}); err == nil {
		t.Error("want error for empty source branch")
	}
	if _, err := c.CreateMergeRequest(context.Background(), 42, CreateMergeRequestParams{SourceBranch: "s", Title: "T"}); err == nil {
		t.Error("want error for empty target branch")
	}
	if _, err := c.CreateMergeRequest(context.Background(), 42, CreateMergeRequestParams{SourceBranch: "s", TargetBranch: "t"}); err == nil {
		t.Error("want error for empty title")
	}
}

// --- GetMergeRequest --------------------------------------------------------

func TestGetMergeRequest_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if rec.path != "/api/v4/projects/42/merge_requests/7" {
			t.Errorf("path = %s", rec.path)
		}
		return jsonResponse(http.StatusOK, `{"iid":7,"state":"merged","merge_commit_sha":"mc1","target_branch":"main"}`), nil
	})
	mr, err := c.GetMergeRequest(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetMergeRequest: %v", err)
	}
	if mr.State != "merged" || mr.MergeCommitSHA != "mc1" || mr.TargetBranch != "main" {
		t.Errorf("mr = %+v", mr)
	}
}

func TestGetMergeRequest_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404 Not Found"}`), nil
	})
	_, err := c.GetMergeRequest(context.Background(), 42, 7)
	assertAPIError(t, err, http.StatusNotFound)
}

func TestGetMergeRequest_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.GetMergeRequest(context.Background(), 0, 7); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.GetMergeRequest(context.Background(), 42, 0); err == nil {
		t.Error("want error for missing iid")
	}
}

// --- UpdateMergeRequest -----------------------------------------------------

func TestUpdateMergeRequest_EditDescription(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodPut {
			t.Errorf("method = %s, want PUT", rec.method)
		}
		if rec.path != "/api/v4/projects/42/merge_requests/7" {
			t.Errorf("path = %s", rec.path)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// A non-nil (even empty) description is sent; state_event absent.
		if got["description"] != "new body" {
			t.Errorf("description = %v", got["description"])
		}
		if _, ok := got["state_event"]; ok {
			t.Error("state_event present though unset")
		}
		return jsonResponse(http.StatusOK, `{"iid":7,"description":"new body"}`), nil
	})
	body := "new body"
	mr, err := c.UpdateMergeRequest(context.Background(), 42, 7, UpdateMergeRequestParams{Description: &body})
	if err != nil {
		t.Fatalf("UpdateMergeRequest: %v", err)
	}
	if mr.Description != "new body" {
		t.Errorf("description = %q", mr.Description)
	}
}

func TestUpdateMergeRequest_CloseStateEvent(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got["state_event"] != "close" {
			t.Errorf("state_event = %v, want close", got["state_event"])
		}
		if _, ok := got["description"]; ok {
			t.Error("description present though nil")
		}
		return jsonResponse(http.StatusOK, `{"iid":7,"state":"closed"}`), nil
	})
	mr, err := c.UpdateMergeRequest(context.Background(), 42, 7, UpdateMergeRequestParams{StateEvent: "close"})
	if err != nil {
		t.Fatalf("UpdateMergeRequest: %v", err)
	}
	if mr.State != "closed" {
		t.Errorf("state = %q", mr.State)
	}
}

func TestUpdateMergeRequest_EmptyBodyIsError(t *testing.T) {
	// No fields to update must not reach the transport.
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called with no fields to update")
		return nil, nil
	})
	if _, err := c.UpdateMergeRequest(context.Background(), 42, 7, UpdateMergeRequestParams{}); err == nil {
		t.Error("want error for empty update")
	}
}

func TestUpdateMergeRequest_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	body := "b"
	if _, err := c.UpdateMergeRequest(context.Background(), 0, 7, UpdateMergeRequestParams{Description: &body}); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.UpdateMergeRequest(context.Background(), 42, 0, UpdateMergeRequestParams{Description: &body}); err == nil {
		t.Error("want error for missing iid")
	}
}

// --- ListMergeRequests ------------------------------------------------------

func TestListMergeRequests_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if rec.path != "/api/v4/projects/42/merge_requests" {
			t.Errorf("path = %s", rec.path)
		}
		q := mustQuery(t, rec.rawQuery)
		if q.Get("state") != "opened" || q.Get("source_branch") != "run/x" || q.Get("target_branch") != "main" {
			t.Errorf("query = %s", rec.rawQuery)
		}
		return jsonResponse(http.StatusOK, `[{"iid":7,"web_url":"u7"},{"iid":8,"web_url":"u8"}]`), nil
	})
	mrs, err := c.ListMergeRequests(context.Background(), 42, ListMergeRequestsParams{
		State: "opened", SourceBranch: "run/x", TargetBranch: "main",
	})
	if err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
	if len(mrs) != 2 || mrs[0].IID != 7 || mrs[1].IID != 8 {
		t.Errorf("mrs = %+v", mrs)
	}
}

func TestListMergeRequests_OmitsEmptyFilters(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.rawQuery != "" {
			t.Errorf("rawQuery = %q, want empty (no filters)", rec.rawQuery)
		}
		return jsonResponse(http.StatusOK, `[]`), nil
	})
	if _, err := c.ListMergeRequests(context.Background(), 42, ListMergeRequestsParams{}); err != nil {
		t.Fatalf("ListMergeRequests: %v", err)
	}
}

func TestListMergeRequests_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusForbidden, `{"message":"403"}`), nil
	})
	_, err := c.ListMergeRequests(context.Background(), 42, ListMergeRequestsParams{State: "opened"})
	assertAPIError(t, err, http.StatusForbidden)
}

func TestListMergeRequests_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.ListMergeRequests(context.Background(), 0, ListMergeRequestsParams{}); err == nil {
		t.Error("want error for missing project id")
	}
}

// --- ListMergeRequestsForCommit ---------------------------------------------

func TestListMergeRequestsForCommit_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if rec.path != "/api/v4/projects/42/repository/commits/deadbeef/merge_requests" {
			t.Errorf("path = %s", rec.path)
		}
		return jsonResponse(http.StatusOK, `[{"iid":7,"title":"t7","web_url":"u7"}]`), nil
	})
	mrs, err := c.ListMergeRequestsForCommit(context.Background(), 42, "deadbeef")
	if err != nil {
		t.Fatalf("ListMergeRequestsForCommit: %v", err)
	}
	if len(mrs) != 1 || mrs[0].Title != "t7" {
		t.Errorf("mrs = %+v", mrs)
	}
}

func TestListMergeRequestsForCommit_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404"}`), nil
	})
	_, err := c.ListMergeRequestsForCommit(context.Background(), 42, "deadbeef")
	assertAPIError(t, err, http.StatusNotFound)
}

func TestListMergeRequestsForCommit_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.ListMergeRequestsForCommit(context.Background(), 0, "sha"); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.ListMergeRequestsForCommit(context.Background(), 42, " "); err == nil {
		t.Error("want error for empty sha")
	}
}

// --- MergeMergeRequest ------------------------------------------------------

func TestMergeMergeRequest_SquashRequestShape(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodPut {
			t.Errorf("method = %s, want PUT", rec.method)
		}
		if rec.path != "/api/v4/projects/42/merge_requests/7/merge" {
			t.Errorf("path = %s", rec.path)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got["squash"] != true {
			t.Errorf("squash = %v, want true", got["squash"])
		}
		if _, ok := got["merge_when_pipeline_succeeds"]; ok {
			t.Error("merge_when_pipeline_succeeds present though unset")
		}
		return jsonResponse(http.StatusOK, `{"iid":7,"state":"merged","merge_commit_sha":"mc"}`), nil
	})
	mr, err := c.MergeMergeRequest(context.Background(), 42, 7, MergeMergeRequestParams{Squash: true})
	if err != nil {
		t.Fatalf("MergeMergeRequest: %v", err)
	}
	if mr.MergeCommitSHA != "mc" {
		t.Errorf("merge_commit_sha = %q", mr.MergeCommitSHA)
	}
}

func TestMergeMergeRequest_AutoMergeRequestShape(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got["merge_when_pipeline_succeeds"] != true {
			t.Errorf("merge_when_pipeline_succeeds = %v, want true", got["merge_when_pipeline_succeeds"])
		}
		return jsonResponse(http.StatusOK, `{"iid":7}`), nil
	})
	if _, err := c.MergeMergeRequest(context.Background(), 42, 7, MergeMergeRequestParams{MergeWhenPipelineSucceeds: true}); err != nil {
		t.Fatalf("MergeMergeRequest: %v", err)
	}
}

func TestMergeMergeRequest_NoParamsSendsNoBody(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if len(rec.body) != 0 {
			t.Errorf("body = %s, want empty", rec.body)
		}
		if ct := rec.header.Get("Content-Type"); ct != "" {
			t.Errorf("Content-Type = %q, want empty for bodyless PUT", ct)
		}
		return jsonResponse(http.StatusOK, `{"iid":7}`), nil
	})
	if _, err := c.MergeMergeRequest(context.Background(), 42, 7, MergeMergeRequestParams{}); err != nil {
		t.Fatalf("MergeMergeRequest: %v", err)
	}
}

func TestMergeMergeRequest_NotMergeable405IsAPIError(t *testing.T) {
	// 405 (not mergeable) surfaces so the adapter maps it to
	// forge.ErrPullRequestNotMergeable.
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusMethodNotAllowed, `{"message":"405 Method Not Allowed"}`), nil
	})
	_, err := c.MergeMergeRequest(context.Background(), 42, 7, MergeMergeRequestParams{})
	assertAPIError(t, err, http.StatusMethodNotAllowed)
}

func TestMergeMergeRequest_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.MergeMergeRequest(context.Background(), 0, 7, MergeMergeRequestParams{}); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.MergeMergeRequest(context.Background(), 42, 0, MergeMergeRequestParams{}); err == nil {
		t.Error("want error for missing iid")
	}
}

// --- SetCommitStatus --------------------------------------------------------

func TestSetCommitStatus_SendsNameAsIdentity(t *testing.T) {
	// Binding condition (1): the status identity rides the `name` parameter,
	// not GitLab's default label, so the check identity is preserved.
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodPost {
			t.Errorf("method = %s, want POST", rec.method)
		}
		if rec.path != "/api/v4/projects/42/statuses/headsha" {
			t.Errorf("path = %s", rec.path)
		}
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// The identity MUST be present and carried as `name`.
		if got["name"] != "fishhawk/plan" {
			t.Errorf("name = %v, want fishhawk/plan", got["name"])
		}
		// It must NOT be sent under the legacy `context` alias.
		if _, ok := got["context"]; ok {
			t.Error("context present; identity must ride `name`, not `context`")
		}
		if got["state"] != "success" {
			t.Errorf("state = %v, want success", got["state"])
		}
		if got["target_url"] != "https://ci/run/1" {
			t.Errorf("target_url = %v", got["target_url"])
		}
		if got["description"] != "all good" {
			t.Errorf("description = %v", got["description"])
		}
		return jsonResponse(http.StatusCreated,
			`{"id":11,"sha":"headsha","status":"success","name":"fishhawk/plan"}`), nil
	})
	st, err := c.SetCommitStatus(context.Background(), 42, "headsha", SetCommitStatusParams{
		State: "success", Name: "fishhawk/plan", TargetURL: "https://ci/run/1", Description: "all good",
	})
	if err != nil {
		t.Fatalf("SetCommitStatus: %v", err)
	}
	if st.Status != "success" || st.Name != "fishhawk/plan" {
		t.Errorf("status = %+v", st)
	}
}

func TestSetCommitStatus_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"message":"invalid state"}`), nil
	})
	_, err := c.SetCommitStatus(context.Background(), 42, "sha", SetCommitStatusParams{State: "bogus", Name: "n"})
	assertAPIError(t, err, http.StatusBadRequest)
}

func TestSetCommitStatus_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.SetCommitStatus(context.Background(), 0, "sha", SetCommitStatusParams{State: "success"}); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.SetCommitStatus(context.Background(), 42, " ", SetCommitStatusParams{State: "success"}); err == nil {
		t.Error("want error for empty sha")
	}
	if _, err := c.SetCommitStatus(context.Background(), 42, "sha", SetCommitStatusParams{State: " "}); err == nil {
		t.Error("want error for empty state")
	}
}

// --- GetProtectedBranch -----------------------------------------------------

func TestGetProtectedBranch_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if want := "/api/v4/projects/42/protected_branches/main"; rec.escapedPath != want {
			t.Errorf("escaped path = %s, want %s", rec.escapedPath, want)
		}
		return jsonResponse(http.StatusOK, `{"id":3,"name":"main"}`), nil
	})
	pb, err := c.GetProtectedBranch(context.Background(), 42, "main")
	if err != nil {
		t.Fatalf("GetProtectedBranch: %v", err)
	}
	if pb.Name != "main" {
		t.Errorf("protected branch = %+v", pb)
	}
}

func TestGetProtectedBranch_NotFoundIsAPIError(t *testing.T) {
	// No protection configured is a 404 the adapter maps to "no classic
	// protection" (ADR-017).
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404 Not Found"}`), nil
	})
	_, err := c.GetProtectedBranch(context.Background(), 42, "main")
	assertAPIError(t, err, http.StatusNotFound)
}

func TestGetProtectedBranch_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.GetProtectedBranch(context.Background(), 0, "main"); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.GetProtectedBranch(context.Background(), 42, " "); err == nil {
		t.Error("want error for empty branch")
	}
}

// --- Compare ----------------------------------------------------------------

func TestCompare_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if rec.path != "/api/v4/projects/42/repository/compare" {
			t.Errorf("path = %s", rec.path)
		}
		q := mustQuery(t, rec.rawQuery)
		if q.Get("from") != "main" || q.Get("to") != "run/x" {
			t.Errorf("from/to = %s", rec.rawQuery)
		}
		// straight=false gives merge-base (three-dot) semantics.
		if q.Get("straight") != "false" {
			t.Errorf("straight = %q, want false", q.Get("straight"))
		}
		return jsonResponse(http.StatusOK, `{
			"commit":{"id":"headsha"},
			"commits":[{"id":"c1"},{"id":"headsha"}],
			"diffs":[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-x\n+y\n"}],
			"compare_timeout":false
		}`), nil
	})
	cmp, err := c.Compare(context.Background(), 42, "main", "run/x", false)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Commit == nil || cmp.Commit.ID != "headsha" {
		t.Errorf("commit = %+v", cmp.Commit)
	}
	if len(cmp.Diffs) != 1 || cmp.Diffs[0].NewPath != "a.go" {
		t.Errorf("diffs = %+v", cmp.Diffs)
	}
	if cmp.CompareTimeout {
		t.Error("compare_timeout = true, want false")
	}
}

func TestCompare_TimeoutFlagDecoded(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"commit":null,"diffs":[],"compare_timeout":true}`), nil
	})
	cmp, err := c.Compare(context.Background(), 42, "main", "run/x", false)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !cmp.CompareTimeout {
		t.Error("compare_timeout = false, want true (adapter maps to Truncated)")
	}
}

func TestCompare_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404"}`), nil
	})
	_, err := c.Compare(context.Background(), 42, "main", "run/x", false)
	assertAPIError(t, err, http.StatusNotFound)
}

func TestCompare_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.Compare(context.Background(), 0, "a", "b", false); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.Compare(context.Background(), 42, " ", "b", false); err == nil {
		t.Error("want error for empty from")
	}
	if _, err := c.Compare(context.Background(), 42, "a", " ", false); err == nil {
		t.Error("want error for empty to")
	}
}

// --- GetCommit --------------------------------------------------------------

func TestGetCommit_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if rec.path != "/api/v4/projects/42/repository/commits/deadbeef" {
			t.Errorf("path = %s", rec.path)
		}
		return jsonResponse(http.StatusOK, `{"id":"deadbeef","short_id":"dead","title":"t","parent_ids":["p1"]}`), nil
	})
	cm, err := c.GetCommit(context.Background(), 42, "deadbeef")
	if err != nil {
		t.Fatalf("GetCommit: %v", err)
	}
	if cm.ID != "deadbeef" || len(cm.ParentIDs) != 1 || cm.ParentIDs[0] != "p1" {
		t.Errorf("commit = %+v", cm)
	}
}

func TestGetCommit_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404"}`), nil
	})
	_, err := c.GetCommit(context.Background(), 42, "deadbeef")
	assertAPIError(t, err, http.StatusNotFound)
}

func TestGetCommit_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.GetCommit(context.Background(), 0, "sha"); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.GetCommit(context.Background(), 42, " "); err == nil {
		t.Error("want error for empty sha")
	}
}

// --- GetProjectByID ---------------------------------------------------------

func TestGetProjectByID_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if rec.path != "/api/v4/projects/42" {
			t.Errorf("path = %s", rec.path)
		}
		return jsonResponse(http.StatusOK,
			`{"id":42,"web_url":"https://gl/g/p","default_branch":"main","path_with_namespace":"g/p"}`), nil
	})
	pi, err := c.GetProjectByID(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProjectByID: %v", err)
	}
	if pi.ID != 42 || pi.DefaultBranch != "main" || pi.PathWithNamespace != "g/p" {
		t.Errorf("project = %+v", pi)
	}
}

func TestGetProjectByID_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusNotFound, `{"message":"404 Project Not Found"}`), nil
	})
	_, err := c.GetProjectByID(context.Background(), 42)
	assertAPIError(t, err, http.StatusNotFound)
}

func TestGetProjectByID_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid project id")
		return nil, nil
	})
	if _, err := c.GetProjectByID(context.Background(), 0); err == nil {
		t.Error("want error for missing project id")
	}
}
