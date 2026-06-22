package corpusdistill

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
