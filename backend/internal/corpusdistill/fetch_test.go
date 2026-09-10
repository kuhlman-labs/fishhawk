package corpusdistill

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestFetchStageTrace_RequestAndDistill covers mode (7): the request path is
// exactly /v0/stages/{id}/trace, the Authorization: Bearer header is sent,
// and a gzipped response body flows through FetchStageTrace into Distill to
// produce a valid case dir (the network->parse->score->filesystem seam).
func TestFetchStageTrace_RequestAndDistill(t *testing.T) {
	const stageID = "11111111-2222-3333-4444-555555555555"
	const token = "fhk_test_token"
	gz := gzipFixture(t, fixtureJSONL)

	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/x-ndjson")
		// Advertise gzip but write raw gzipped bytes; httptest's client
		// will transparently decompress, but Distill auto-detects either
		// way.
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gz)
	}))
	defer srv.Close()

	body, err := FetchStageTrace(context.Background(), srv.URL, stageID, token)
	if err != nil {
		t.Fatalf("FetchStageTrace: %v", err)
	}
	if want := "/v0/stages/" + stageID + "/trace"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}

	dir := t.TempDir()
	caseDir, err := Distill(bytes.NewReader(body), Options{CaseName: "fetched", Issue: "#1290", OutDir: dir})
	if err != nil {
		t.Fatalf("Distill fetched body: %v", err)
	}
	files := readDir(t, caseDir)
	if len(files["trace.jsonl"]) == 0 {
		t.Error("fetched case produced empty trace.jsonl")
	}
}

// TestFetchStageTrace_NoToken asserts the Authorization header is omitted
// when token is empty.
func TestFetchStageTrace_NoToken(t *testing.T) {
	gotAuth := "sentinel"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write(gzipFixture(t, fixtureJSONL))
	}))
	defer srv.Close()

	if _, err := FetchStageTrace(context.Background(), srv.URL, "sid", ""); err != nil {
		t.Fatalf("FetchStageTrace: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header set with empty token: %q", gotAuth)
	}
}

// TestFetchStageTrace_NonOK covers mode (8): a non-200 response yields a
// clear error naming the status code.
func TestFetchStageTrace_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"trace_not_found"}`))
	}))
	defer srv.Close()

	_, err := FetchStageTrace(context.Background(), srv.URL, "sid", "")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error does not name the status: %v", err)
	}
}

// TestFetchStageTrace_TransportError covers the transport-error branch:
// http.DefaultClient.Do returns an error (here, an unreachable backend whose
// listener was closed before the request) and FetchStageTrace must surface it
// wrapped as a GET failure rather than panic on a nil response.
func TestFetchStageTrace_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // close so the address is no longer accepting connections

	_, err := FetchStageTrace(context.Background(), url, "sid", "")
	if err == nil {
		t.Fatal("expected transport error against a closed server, got nil")
	}
	if !strings.Contains(err.Error(), "GET ") {
		t.Errorf("transport error not wrapped as a GET failure: %v", err)
	}
}

// TestFetchRunTriageAudit_SinglePage: the request path + query pin
// (category=acceptance_triage_decided&limit=500), the Bearer header, and a
// single-page decode with an empty next_cursor.
func TestFetchRunTriageAudit_SinglePage(t *testing.T) {
	const runID = "11111111-2222-3333-4444-555555555555"
	const token = "fhk_test_token"
	var gotPath, gotQuery, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"items":[{"sequence":9,"run_id":"` + runID + `","ts":"2026-07-01T00:00:00Z","payload":{"class":"3"}}],"next_cursor":""}`))
	}))
	defer srv.Close()

	items, err := FetchRunTriageAudit(context.Background(), srv.URL, runID, token)
	if err != nil {
		t.Fatalf("FetchRunTriageAudit: %v", err)
	}
	if want := "/v0/runs/" + runID + "/audit"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	for _, want := range []string{"category=acceptance_triage_decided", "limit=500"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
	if want := "Bearer " + token; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
	if len(items) != 1 || items[0].Sequence != 9 || items[0].RunID != runID {
		t.Errorf("items = %+v", items)
	}
}

// TestFetchRunTriageAudit_FollowsNextCursor pins the pagination contract:
// the audit endpoint caps limit at 500, so the fetch must follow next_cursor
// pages rather than silently truncating a long audit chain.
func TestFetchRunTriageAudit_FollowsNextCursor(t *testing.T) {
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		cursors = append(cursors, cursor)
		switch cursor {
		case "":
			_, _ = w.Write([]byte(`{"items":[{"sequence":1,"payload":{}}],"next_cursor":"page-2"}`))
		case "page-2":
			_, _ = w.Write([]byte(`{"items":[{"sequence":2,"payload":{}}],"next_cursor":""}`))
		default:
			t.Errorf("unexpected cursor %q", cursor)
		}
	}))
	defer srv.Close()

	items, err := FetchRunTriageAudit(context.Background(), srv.URL, "rid", "")
	if err != nil {
		t.Fatalf("FetchRunTriageAudit: %v", err)
	}
	if len(items) != 2 || items[0].Sequence != 1 || items[1].Sequence != 2 {
		t.Errorf("items across pages = %+v, want sequences [1 2]", items)
	}
	if len(cursors) != 2 || cursors[1] != "page-2" {
		t.Errorf("cursor walk = %v, want [\"\" page-2]", cursors)
	}
}

// TestFetchRunTriageAudit_NonOK: a non-200 response yields a clear error
// naming the status code (the FetchStageTrace error shape).
func TestFetchRunTriageAudit_NonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"run_not_found"}`))
	}))
	defer srv.Close()

	_, err := FetchRunTriageAudit(context.Background(), srv.URL, "rid", "")
	if err == nil {
		t.Fatal("expected error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error does not name the status: %v", err)
	}
}

// TestFetchRunTriageAudit_BadJSON: an undecodable page body errors rather
// than returning a partial silently.
func TestFetchRunTriageAudit_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":`))
	}))
	defer srv.Close()

	_, err := FetchRunTriageAudit(context.Background(), srv.URL, "rid", "")
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode audit page") {
		t.Errorf("error not wrapped as a page-decode failure: %v", err)
	}
}

// ---------------------------------------------------------------------------
// FetchRunConcernDispositions (E50.22 / #3309).
// ---------------------------------------------------------------------------

// TestFetchRunConcernDispositions_MergesCategories drives the load-bearing
// contract: the audit endpoint filters by exactly ONE category per request
// (server/reads.go handleListRunAudit reads a single `category` value), so
// the helper must issue one request PER category and merge the results
// ASCENDING BY SEQUENCE. The server serves a DISTINCT entry per category so
// a single-request implementation — or one assuming a comma-separated or
// repeated parameter works — returns a strict subset and fails here.
func TestFetchRunConcernDispositions_MergesCategories(t *testing.T) {
	const token = "fhk_test_token"
	// Deliberately NOT in fetch order: the merge must sort, not concatenate.
	seqByCategory := map[string]int64{
		"implement_reviewed":             40,
		"concern_waived":                 10,
		"concern_deferred":               30,
		"concern_addressed_by_condition": 20,
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q, want Bearer %s", got, token)
		}
		if got := r.URL.Path; got != "/v0/runs/rid/audit" {
			t.Errorf("path = %q", got)
		}
		cat := r.URL.Query().Get("category")
		seen = append(seen, cat)
		seq, ok := seqByCategory[cat]
		if !ok {
			t.Errorf("unexpected category %q", cat)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"sequence":` +
			strconv.FormatInt(seq, 10) + `,"run_id":"rid","category":"` + cat +
			`","payload":{}}],"next_cursor":""}`))
	}))
	defer srv.Close()

	items, err := FetchRunConcernDispositions(context.Background(), srv.URL, "rid", token)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(seen) != len(CalibrationCategories) {
		t.Fatalf("issued %d requests for %d categories: %v", len(seen), len(CalibrationCategories), seen)
	}
	for _, want := range CalibrationCategories {
		found := false
		for _, got := range seen {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("category %q was never requested", want)
		}
	}
	if len(items) != 4 {
		t.Fatalf("want 4 merged items, got %d", len(items))
	}
	wantOrder := []int64{10, 20, 30, 40}
	for i, want := range wantOrder {
		if items[i].Sequence != want {
			t.Errorf("merged item %d sequence = %d, want %d (the merge must be ascending by sequence)", i, items[i].Sequence, want)
		}
	}
	// The category discriminator must survive the decode: without it the
	// join cannot tell a review verdict from a disposition.
	for _, it := range items {
		if it.Category == "" {
			t.Errorf("item at sequence %d decoded with an empty category", it.Sequence)
		}
	}
}

// TestFetchRunConcernDispositions_FollowsPages: next_cursor is followed
// per category, so a long-audit run cannot silently truncate.
func TestFetchRunConcernDispositions_FollowsPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cat := r.URL.Query().Get("category")
		if cat != "concern_waived" {
			_, _ = w.Write([]byte(`{"items":[],"next_cursor":""}`))
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"items":[{"sequence":1,"category":"concern_waived","payload":{}}],"next_cursor":"c2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"sequence":2,"category":"concern_waived","payload":{}}],"next_cursor":""}`))
	}))
	defer srv.Close()

	items, err := FetchRunConcernDispositions(context.Background(), srv.URL, "rid", "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("want 2 items across both pages, got %d", len(items))
	}
	if items[0].Sequence != 1 || items[1].Sequence != 2 {
		t.Errorf("paged items = %+v", items)
	}
}

// TestFetchRunConcernDispositions_NonOKStatus: a non-200 on ANY ONE of the
// four category requests is an error carrying the status + body snippet.
// The server is a REACHABLE in-test address returning 500 — never an
// unreachable host, whose transport error would map to the same failure
// whether or not the status guard exists.
func TestFetchRunConcernDispositions_NonOKStatus(t *testing.T) {
	for _, bad := range CalibrationCategories {
		t.Run(bad, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("category") == bad {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":"boom"}`))
					return
				}
				_, _ = w.Write([]byte(`{"items":[],"next_cursor":""}`))
			}))
			defer srv.Close()

			_, err := FetchRunConcernDispositions(context.Background(), srv.URL, "rid", "")
			if err == nil {
				t.Fatalf("expected an error when %s returned 500, got nil", bad)
			}
			if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
				t.Errorf("error should carry the status and a body snippet, got: %v", err)
			}
		})
	}
}

// TestFetchRunConcernDispositions_BadJSON: an undecodable page body errors
// rather than returning a partial silently.
func TestFetchRunConcernDispositions_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":`))
	}))
	defer srv.Close()

	_, err := FetchRunConcernDispositions(context.Background(), srv.URL, "rid", "")
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode audit page") {
		t.Errorf("error not wrapped as a page-decode failure: %v", err)
	}
}
