package modeloracle_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
)

// TestAnthropicFetcher_CollectsIDsAcrossPages drives AnthropicFetcher against an
// httptest server emulating the Anthropic /v1/models endpoint with TWO pages,
// asserting ListAutoPaging drains both and the full id set is collected.
func TestAnthropicFetcher_CollectsIDsAcrossPages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		// Page two is requested with ?after_id=<last id of page one>.
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "claude-sonnet-4-6" {
			fmt.Fprint(w, `{"data":[{"id":"claude-haiku-4-5","type":"model"}],"has_more":false,"last_id":"claude-haiku-4-5"}`)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-8","type":"model"},{"id":"claude-sonnet-4-6","type":"model"}],"has_more":true,"last_id":"claude-sonnet-4-6"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := modeloracle.NewAnthropicFetcher("test-key", srv.URL, srv.Client())
	ids, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	sort.Strings(ids)
	want := []string{"claude-haiku-4-5", "claude-opus-4-8", "claude-sonnet-4-6"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", ids, want)
	}
}

// TestAnthropicFetcher_EmptyList asserts an empty data array yields an empty id
// set and no error.
func TestAnthropicFetcher_EmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[],"has_more":false}`)
	}))
	defer srv.Close()

	f := modeloracle.NewAnthropicFetcher("test-key", srv.URL, srv.Client())
	ids, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty", ids)
	}
}

// TestAnthropicFetcher_NonOKError asserts a non-200 from the models endpoint is
// surfaced as an error (so Refresh keeps the prior snapshot).
func TestAnthropicFetcher_NonOKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()

	f := modeloracle.NewAnthropicFetcher("test-key", srv.URL, srv.Client())
	if _, err := f.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch error = nil, want non-nil on a 500")
	}
}

// TestOpenAIFetcher_CollectsIDs drives OpenAIFetcher against an httptest server
// returning the OpenAI list-models shape, asserting id extraction and that the
// Bearer auth header is sent.
func TestOpenAIFetcher_CollectsIDs(t *testing.T) {
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5.5","object":"model"},{"id":"gpt-4o","object":"model"}]}`)
	}))
	defer srv.Close()

	f := modeloracle.NewOpenAIFetcher("sk-test", srv.URL, srv.Client())
	ids, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	sort.Strings(ids)
	want := []string{"gpt-4o", "gpt-5.5"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v", ids, want)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}
	if gotPath != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", gotPath)
	}
}

// TestOpenAIFetcher_EmptyList asserts an empty data array yields an empty set.
func TestOpenAIFetcher_EmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer srv.Close()

	f := modeloracle.NewOpenAIFetcher("sk-test", srv.URL, srv.Client())
	ids, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("ids = %v, want empty", ids)
	}
}

// TestOpenAIFetcher_NonOKError asserts a non-200 status is surfaced as an error.
func TestOpenAIFetcher_NonOKError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"invalid key"}}`)
	}))
	defer srv.Close()

	f := modeloracle.NewOpenAIFetcher("sk-test", srv.URL, srv.Client())
	_, err := f.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch error = nil, want non-nil on a 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q does not mention the status code", err.Error())
	}
}
