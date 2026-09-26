package gitlabclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// TestGetMergeRequest_DecodesMergedAt pins the E64.40 / #3151 merge-evidence
// decode: merged_at is a *time.Time so an unmerged (or timestamp-omitting) MR
// stays a NIL pointer, distinguishable from the zero time. A value-typed field
// would decode a null/absent merged_at to the Unix epoch and silently satisfy
// the observe verb's rung-10 merged-timestamp presence gate.
func TestGetMergeRequest_DecodesMergedAt(t *testing.T) {
	t.Run("merged: merged_at decodes to a non-nil timestamp", func(t *testing.T) {
		c := clientWith(t, func(*opRequest) (*http.Response, error) {
			return jsonResponse(http.StatusOK,
				`{"iid":7,"state":"merged","merge_commit_sha":"mc1","merged_at":"2026-08-30T12:34:56Z"}`), nil
		})
		mr, err := c.GetMergeRequest(context.Background(), 42, 7)
		if err != nil {
			t.Fatalf("GetMergeRequest: %v", err)
		}
		if mr.MergedAt == nil {
			t.Fatalf("MergedAt = nil, want the decoded merge timestamp")
		}
		want := time.Date(2026, 8, 30, 12, 34, 56, 0, time.UTC)
		if !mr.MergedAt.Equal(want) {
			t.Errorf("MergedAt = %v, want %v", mr.MergedAt.UTC(), want)
		}
	})
	t.Run("null merged_at stays nil, not the zero time", func(t *testing.T) {
		c := clientWith(t, func(*opRequest) (*http.Response, error) {
			return jsonResponse(http.StatusOK,
				`{"iid":7,"state":"opened","merge_commit_sha":null,"merged_at":null}`), nil
		})
		mr, err := c.GetMergeRequest(context.Background(), 42, 7)
		if err != nil {
			t.Fatalf("GetMergeRequest: %v", err)
		}
		if mr.MergedAt != nil {
			t.Errorf("MergedAt = %v, want nil (a null merged_at must not decode to the zero time)", mr.MergedAt.UTC())
		}
	})
	t.Run("absent merged_at key stays nil", func(t *testing.T) {
		c := clientWith(t, func(*opRequest) (*http.Response, error) {
			return jsonResponse(http.StatusOK, `{"iid":7,"state":"opened"}`), nil
		})
		mr, err := c.GetMergeRequest(context.Background(), 42, 7)
		if err != nil {
			t.Fatalf("GetMergeRequest: %v", err)
		}
		if mr.MergedAt != nil {
			t.Errorf("MergedAt = %v, want nil when the key is absent", mr.MergedAt.UTC())
		}
	})
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

// --- ListProtectedBranches (E45.66 / #3580) ---------------------------------

// protectedRulesPage1/2 are literal documented list bodies
// (https://docs.gitlab.com/api/protected_branches/#list-protected-branches);
// a wrong json tag zero-values the decoded field and fails the assertion.
const (
	protectedRulesPage1 = `[{"id":1,"name":"main",` +
		`"push_access_levels":[{"access_level":0,"access_level_description":"No one"}],` +
		`"merge_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],` +
		`"allow_force_push":false}]`
	protectedRulesPage2 = `[{"id":2,"name":"release-*",` +
		`"push_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],` +
		`"merge_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],` +
		`"allow_force_push":true}]`
)

func TestListProtectedBranches_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodGet {
			t.Errorf("method = %s, want GET", rec.method)
		}
		if want := "/api/v4/projects/42/protected_branches"; rec.escapedPath != want {
			t.Errorf("escaped path = %s, want %s", rec.escapedPath, want)
		}
		if q := mustQuery(t, rec.rawQuery); q.Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", q.Get("per_page"))
		}
		if rec.header.Get("PRIVATE-TOKEN") != testToken {
			t.Errorf("PRIVATE-TOKEN = %q, want %q", rec.header.Get("PRIVATE-TOKEN"), testToken)
		}
		return jsonResponse(http.StatusOK, protectedRulesPage1), nil
	})
	rules, err := c.ListProtectedBranches(context.Background(), 42)
	if err != nil {
		t.Fatalf("ListProtectedBranches: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("len(rules) = %d, want 1", len(rules))
	}
	r := rules[0]
	if r.ID != 1 || r.Name != "main" || r.AllowForcePush {
		t.Errorf("rule = %+v, want id 1 / main / allow_force_push false", r)
	}
	if len(r.PushAccessLevels) != 1 || r.PushAccessLevels[0].AccessLevel != 0 || r.PushAccessLevels[0].AccessLevelDescription != "No one" {
		t.Errorf("push_access_levels = %+v, want [{0 No one}]", r.PushAccessLevels)
	}
	if len(r.MergeAccessLevels) != 1 || r.MergeAccessLevels[0].AccessLevel != 40 || r.MergeAccessLevels[0].AccessLevelDescription != "Maintainers" {
		t.Errorf("merge_access_levels = %+v, want [{40 Maintainers}]", r.MergeAccessLevels)
	}
}

// TestListProtectedBranches_WalksNextPageLink is the counterfactual vehicle
// for the Link-header paging loop: page 1 carries a rel="next" Link to page
// 2 and the result must carry BOTH pages in order. Returning after the first
// page drops the release-* rule and reddens the length assertion.
func TestListProtectedBranches_WalksNextPageLink(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link",
				`<`+s.srv.URL+`/api/v4/projects/42/protected_branches?page=2&per_page=100>; rel="next", `+
					`<`+s.srv.URL+`/api/v4/projects/42/protected_branches?page=1&per_page=100>; rel="first"`)
			writeIssueJSON(w, http.StatusOK, protectedRulesPage1)
		case "2":
			w.Header().Set("Link", `<`+s.srv.URL+`/api/v4/projects/42/protected_branches?page=1&per_page=100>; rel="prev"`)
			writeIssueJSON(w, http.StatusOK, protectedRulesPage2)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			writeIssueJSON(w, http.StatusBadRequest, `{}`)
		}
	})

	rules, err := s.client().ListProtectedBranches(context.Background(), 42)
	if err != nil {
		t.Fatalf("ListProtectedBranches: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("len(rules) = %d, want 2 (both pages walked)", len(rules))
	}
	if rules[0].Name != "main" || rules[1].Name != "release-*" {
		t.Errorf("rules = [%q %q], want [main release-*] in page order", rules[0].Name, rules[1].Name)
	}
	if !rules[1].AllowForcePush {
		t.Error("rules[1].AllowForcePush = false, want true (page-2 rule decoded intact)")
	}
	reqs := s.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (one per page)", len(reqs))
	}
	if !strings.Contains(reqs[0].Query, "per_page=100") {
		t.Errorf("first page query = %q, want per_page=100", reqs[0].Query)
	}
	if !strings.Contains(reqs[1].Query, "page=2") {
		t.Errorf("second request query = %q, want the Link's page=2", reqs[1].Query)
	}
	for i, r := range reqs {
		if r.Token != "glpat-test" {
			t.Errorf("request %d PRIVATE-TOKEN = %q, want glpat-test on every page", i, r.Token)
		}
	}
}

// TestListProtectedBranches_RefusesOffOriginNextLink pins the same-origin
// guard on the Link walk (mirror of
// TestGitLabClient_ListIssueNotes_RefusesOffOriginNextLink): a rel="next"
// pointing at a DIFFERENT host is refused and that host is never dialed. The
// foreign host is a reachable in-test server so that deleting the
// sameOrigin call SUCCEEDS against it (and reddens the zero-hit assertion)
// rather than failing on a connection error.
func TestListProtectedBranches_RefusesOffOriginNextLink(t *testing.T) {
	var foreignHits atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits.Add(1)
		writeIssueJSON(w, http.StatusOK, `[]`)
	}))
	t.Cleanup(foreign.Close)

	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<`+foreign.URL+`/api/v4/projects/42/protected_branches?page=2>; rel="next"`)
		writeIssueJSON(w, http.StatusOK, protectedRulesPage1)
	})

	rules, err := s.client().ListProtectedBranches(context.Background(), 42)
	if err == nil {
		t.Fatalf("ListProtectedBranches = %v, nil; want a refusal of the off-origin next link", rules)
	}
	if !strings.Contains(err.Error(), "refusing next-page link") {
		t.Errorf("err = %v, want the same-origin refusal", err)
	}
	if got := foreignHits.Load(); got != 0 {
		t.Errorf("foreign host received %d requests, want 0 (token must not leave the configured origin)", got)
	}
}

// TestListProtectedBranches_PageCapFailsClosed pins the fail-closed page cap
// (#3591): a server that ALWAYS emits a same-origin rel="next" link would
// spin the walk forever, so the loop refuses at maxListPages rather than
// returning a silently-partial rule set. Fail-closed matters most here — a
// truncated protected-branch rule list would let the adapter find no matching
// rule and report an AUTHORITATIVE Protected:false, read downstream as
// "unprotected". Asserts: a naming error (cap + accumulated count), a nil
// slice, and exactly maxListPages requests (the cap, not the client, stopped
// it).
func TestListProtectedBranches_PageCapFailsClosed(t *testing.T) {
	s := newIssueServer(t)
	s.mux.HandleFunc("GET /api/v4/projects/42/protected_branches", func(w http.ResponseWriter, r *http.Request) {
		// Always advertise a next page, whatever the current page.
		w.Header().Set("Link", `<`+s.srv.URL+`/api/v4/projects/42/protected_branches?page=99&per_page=100>; rel="next"`)
		writeIssueJSON(w, http.StatusOK, protectedRulesPage1)
	})

	rules, err := s.client().ListProtectedBranches(context.Background(), 42)
	if err == nil {
		t.Fatalf("ListProtectedBranches = %v, nil; want a fail-closed page-cap error", rules)
	}
	if rules != nil {
		t.Errorf("rules = %v, want nil (no silently-partial set on the cap)", rules)
	}
	if !strings.Contains(err.Error(), "page cap") {
		t.Errorf("err = %v, want it to name the page cap", err)
	}
	if !strings.Contains(err.Error(), "partial protected-branch rule set") {
		t.Errorf("err = %v, want it to name the refused partial set", err)
	}
	if n := len(s.requests()); n != maxListPages {
		t.Errorf("requests = %d, want exactly maxListPages (%d) — the cap, not an early give-up, must stop the walk", n, maxListPages)
	}
}

func TestListProtectedBranches_APIError(t *testing.T) {
	// GitLab documents the endpoint as Maintainer-only: a credential that
	// can read the project may still draw a 403 here, which the adapter
	// maps to ErrForbidden (never "unprotected").
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusForbidden, `{"message":"403 Forbidden"}`), nil
	})
	_, err := c.ListProtectedBranches(context.Background(), 42)
	assertAPIError(t, err, http.StatusForbidden)
}

func TestListProtectedBranches_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid project id")
		return nil, nil
	})
	if _, err := c.ListProtectedBranches(context.Background(), 0); err == nil {
		t.Error("want error for missing project id")
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
		// The two merge-requirement settings ride on the same response
		// (E45.55 / #3490); their JSON names are pinned here because they
		// are config-shaped strings no compiler checks.
		return jsonResponse(http.StatusOK,
			`{"id":42,"web_url":"https://gl/g/p","default_branch":"main","path_with_namespace":"g/p",`+
				`"only_allow_merge_if_pipeline_succeeds":true,"allow_merge_on_skipped_pipeline":true,`+
				`"only_allow_merge_if_all_discussions_are_resolved":true}`), nil
	})
	pi, err := c.GetProjectByID(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetProjectByID: %v", err)
	}
	if pi.ID != 42 || pi.DefaultBranch != "main" || pi.PathWithNamespace != "g/p" {
		t.Errorf("project = %+v", pi)
	}
	if !pi.OnlyAllowMergeIfPipelineSucceeds {
		t.Error("OnlyAllowMergeIfPipelineSucceeds = false, want true (json only_allow_merge_if_pipeline_succeeds)")
	}
	if !pi.AllowMergeOnSkippedPipeline {
		t.Error("AllowMergeOnSkippedPipeline = false, want true (json allow_merge_on_skipped_pipeline)")
	}
	if !pi.OnlyAllowMergeIfAllDiscussionsAreResolved {
		t.Error("OnlyAllowMergeIfAllDiscussionsAreResolved = false, want true (json only_allow_merge_if_all_discussions_are_resolved)")
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

// --- CreatePipeline ---------------------------------------------------------

func TestCreatePipeline_RequestShapeAndResult(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		if rec.method != http.MethodPost {
			t.Errorf("method = %s, want POST", rec.method)
		}
		if rec.path != "/api/v4/projects/42/pipeline" {
			t.Errorf("path = %s", rec.path)
		}
		assertPrivateToken(t, rec.header)
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got["ref"] != "fishhawk/run-abc12345/slice-2" {
			t.Errorf("ref = %v, want the slice branch", got["ref"])
		}
		// variables is an ARRAY of {key,value} objects (not a flat map), in
		// insertion order.
		rawVars, ok := got["variables"].([]any)
		if !ok {
			t.Fatalf("variables not an array: %T", got["variables"])
		}
		if len(rawVars) != 2 {
			t.Fatalf("variables len = %d, want 2", len(rawVars))
		}
		first, _ := rawVars[0].(map[string]any)
		if first["key"] != "run_id" || first["value"] != "r1" {
			t.Errorf("variables[0] = %v, want {run_id,r1}", first)
		}
		second, _ := rawVars[1].(map[string]any)
		if second["key"] != "stage" || second["value"] != "claude-code" {
			t.Errorf("variables[1] = %v, want {stage,claude-code}", second)
		}
		return jsonResponse(http.StatusCreated,
			`{"id":900,"sha":"headsha","ref":"fishhawk/run-abc12345/slice-2","status":"created","web_url":"https://gl/pipe/900"}`), nil
	})
	pipe, err := c.CreatePipeline(context.Background(), 42, CreatePipelineParams{
		Ref: "fishhawk/run-abc12345/slice-2",
		Variables: []PipelineVariable{
			{Key: "run_id", Value: "r1"},
			{Key: "stage", Value: "claude-code"},
		},
	})
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	if pipe.ID != 900 || pipe.SHA != "headsha" || pipe.Status != "created" || pipe.WebURL != "https://gl/pipe/900" {
		t.Errorf("pipeline = %+v", pipe)
	}
}

func TestCreatePipeline_OmitsEmptyVariables(t *testing.T) {
	c := clientWith(t, func(rec *opRequest) (*http.Response, error) {
		var got map[string]any
		if err := json.Unmarshal(rec.body, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := got["variables"]; ok {
			t.Error("variables present though empty")
		}
		return jsonResponse(http.StatusCreated, `{"id":1,"ref":"main"}`), nil
	})
	if _, err := c.CreatePipeline(context.Background(), 42, CreatePipelineParams{Ref: "main"}); err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
}

func TestCreatePipeline_APIError(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"message":"Reference not found"}`), nil
	})
	_, err := c.CreatePipeline(context.Background(), 42, CreatePipelineParams{Ref: "nope"})
	assertAPIError(t, err, http.StatusBadRequest)
}

func TestCreatePipeline_ValidatesArgs(t *testing.T) {
	c := clientWith(t, func(*opRequest) (*http.Response, error) {
		t.Fatal("transport called despite invalid args")
		return nil, nil
	})
	if _, err := c.CreatePipeline(context.Background(), 0, CreatePipelineParams{Ref: "main"}); err == nil {
		t.Error("want error for missing project id")
	}
	if _, err := c.CreatePipeline(context.Background(), 42, CreatePipelineParams{Ref: " "}); err == nil {
		t.Error("want error for empty ref")
	}
}
