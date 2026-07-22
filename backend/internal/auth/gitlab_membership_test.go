package auth_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
)

func TestNewGitLabMembershipLister_EmptyBaseURL_Nil(t *testing.T) {
	for _, in := range []string{"", "   "} {
		if l := auth.NewGitLabMembershipLister(in); l != nil {
			t.Errorf("NewGitLabMembershipLister(%q) = %v, want nil (unconfigured ⇒ no lister)", in, l)
		}
	}
}

func TestGitLabMembershipLister_ListUserOrgKeys_DecodesFullPath(t *testing.T) {
	var gotAuth, gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		// "name" is a display string; only full_path is a usable key.
		_, _ = w.Write([]byte(`[
			{"full_path":"acme/platform","name":"Platform"},
			{"full_path":"acme","name":"Acme"},
			{"full_path":"","name":"unusable"}
		]`))
	}))
	defer srv.Close()

	keys, err := auth.NewGitLabMembershipLister(srv.URL).
		ListUserOrgKeys(context.Background(), "gl-user-token")
	if err != nil {
		t.Fatalf("ListUserOrgKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "acme/platform" || keys[1] != "acme" {
		t.Errorf("keys = %v, want the two non-empty full_path values", keys)
	}
	// The USER's OAuth token, never the deployment PRIVATE-TOKEN.
	if gotAuth != "Bearer gl-user-token" {
		t.Errorf("Authorization = %q, want the per-call user token", gotAuth)
	}
	if gotPath != "/api/v4/groups" {
		t.Errorf("path = %q, want /api/v4/groups", gotPath)
	}
	if !strings.Contains(gotQuery, "min_access_level=10") {
		t.Errorf("query = %q, want min_access_level=10", gotQuery)
	}
}

// CONDITION (2): a matching group on a LATER page must still be found.
func TestGitLabMembershipLister_FollowsPagination(t *testing.T) {
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		if page == "1" {
			w.Header().Set("Link", `<http://example.invalid/api/v4/groups?page=2>; rel="next", <http://example.invalid/api/v4/groups?page=2>; rel="last"`)
			_, _ = w.Write([]byte(`[{"full_path":"first/page-group"}]`))
			return
		}
		// Last page: no Link rel="next".
		w.Header().Set("Link", `<http://example.invalid/api/v4/groups?page=1>; rel="prev"`)
		_, _ = w.Write([]byte(`[{"full_path":"acme/platform"}]`))
	}))
	defer srv.Close()

	keys, err := auth.NewGitLabMembershipLister(srv.URL).
		ListUserOrgKeys(context.Background(), "gl-user-token")
	if err != nil {
		t.Fatalf("ListUserOrgKeys: %v", err)
	}
	if len(keys) != 2 || keys[1] != "acme/platform" {
		t.Errorf("keys = %v, want both pages including the page-2 match", keys)
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("requested pages = %v, want [1 2]", pages)
	}
}

// With no Link header the walk falls back to "a full page implies
// another" — and stops on a short page.
func TestGitLabMembershipLister_PaginationWithoutLinkHeader(t *testing.T) {
	var requested int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			var b strings.Builder
			b.WriteString("[")
			for i := range 100 {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"full_path":"g/%d"}`, i)
			}
			b.WriteString("]")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		_, _ = w.Write([]byte(`[{"full_path":"acme/platform"}]`))
	}))
	defer srv.Close()

	keys, err := auth.NewGitLabMembershipLister(srv.URL).
		ListUserOrgKeys(context.Background(), "gl-user-token")
	if err != nil {
		t.Fatalf("ListUserOrgKeys: %v", err)
	}
	if len(keys) != 101 || keys[100] != "acme/platform" {
		t.Errorf("keys = %d entries (last %q), want 101 ending in the page-2 match", len(keys), keys[len(keys)-1])
	}
	if requested != 2 {
		t.Errorf("requested %d pages, want 2 (stop on the short page)", requested)
	}
}

// The pagination walk is bounded: a server that always advertises a
// next page errors out rather than looping unbounded.
func TestGitLabMembershipLister_PaginationCap_FailsClosed(t *testing.T) {
	var requested int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<http://example.invalid/x>; rel="next"`)
		_, _ = w.Write([]byte(`[{"full_path":"g/x"}]`))
	}))
	defer srv.Close()

	keys, err := auth.NewGitLabMembershipLister(srv.URL).
		ListUserOrgKeys(context.Background(), "gl-user-token")
	if err == nil {
		t.Fatalf("ListUserOrgKeys = %v, want an error at the page cap", keys)
	}
	if requested > 60 {
		t.Errorf("requested %d pages, want the walk bounded well under that", requested)
	}
}

func TestGitLabMembershipLister_FailClosedModes(t *testing.T) {
	t.Run("empty access token", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
		defer srv.Close()
		if _, err := auth.NewGitLabMembershipLister(srv.URL).
			ListUserOrgKeys(context.Background(), ""); err == nil {
			t.Error("ListUserOrgKeys with an empty token = nil error, want an error")
		}
		if called {
			t.Error("an empty token still issued a request")
		}
	})
	t.Run("non-200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer srv.Close()
		if _, err := auth.NewGitLabMembershipLister(srv.URL).
			ListUserOrgKeys(context.Background(), "tok"); err == nil {
			t.Error("ListUserOrgKeys on 403 = nil error, want an error")
		}
	})
	t.Run("malformed body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{not json`))
		}))
		defer srv.Close()
		if _, err := auth.NewGitLabMembershipLister(srv.URL).
			ListUserOrgKeys(context.Background(), "tok"); err == nil {
			t.Error("ListUserOrgKeys on a malformed body = nil error, want an error")
		}
	})
	t.Run("transport error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing is listening now
		if _, err := auth.NewGitLabMembershipLister(url).
			ListUserOrgKeys(context.Background(), "tok"); err == nil {
			t.Error("ListUserOrgKeys against a dead server = nil error, want an error")
		}
	})
}
