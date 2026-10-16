package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestPostOrEditStatusComment_ProductionSeamForwardsToken pins the default
// seam wiring (#4483): the token argument reaches the backend as a bearer.
// The fake backend 404s the rendered-body GET, so the call stops before gh.
func TestPostOrEditStatusComment_ProductionSeamForwardsToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := postOrEditStatusComment(srv.URL, "tok-seam", uuid.NewString(), "x/y", 1)
	if err == nil || !strings.Contains(err.Error(), "fetch rendered body") {
		t.Errorf("err = %v, want a fetch rendered body error", err)
	}
	if auth != "Bearer tok-seam" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer tok-seam")
	}
}
