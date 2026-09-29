package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
)

// digestNoIndex is an IndexReader that fails the test if it is ever reached:
// the request-shape guards below must refuse BEFORE any read.
type digestNoIndex struct{ t *testing.T }

func (f digestNoIndex) List(context.Context, decisionindex.ListFilter) ([]decisionindex.Row, error) {
	f.t.Fatal("decision index read reached past a request-shape guard")
	return nil, nil
}

func (f digestNoIndex) GapsInWindow(context.Context, decisionindex.GapFilter) (*decisionindex.GapReport, error) {
	f.t.Fatal("decision index gap read reached past a request-shape guard")
	return nil, nil
}

// digestTokenIdentity is a bearer-token identity carrying exactly scopes.
func digestTokenIdentity(subject string, scopes ...string) *Identity {
	return &Identity{Subject: subject, TokenID: "tok-" + subject, Scopes: scopes}
}

// serveDigest drives one request through the handler for method with id in
// context (nil = no identity, i.e. anonymous).
func serveDigest(t *testing.T, s *Server, method, target, body string, id *Identity) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	w := httptest.NewRecorder()
	if method == http.MethodGet {
		s.handleGetDigest(w, req)
	} else {
		s.handleDigestMarkRead(w, req)
	}
	return w
}

// digestErrorBody decodes the error envelope.
func digestErrorBody(t *testing.T, w *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	return env.Error
}

// TestDigestRoutes_Registered: both routes are on the mux (an anonymous call
// reaches the handler's auth gate — 401 — rather than 404/405).
func TestDigestRoutes_Registered(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	h := s.Handler()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v0/digest?repo=o/r", ""},
		{http.MethodPost, "/v0/digest/mark-read", `{"repo":"o/r","to_sequence":1}`},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (route registered, auth gate reached):\n%s", c.method, c.path, w.Code, w.Body.String())
		}
	}
}

// TestGetDigest_UnconfiguredIs501 / TestMarkRead_UnconfiguredIs501: a nil
// store degrades to a NAMED 501 rather than panicking; mark-read additionally
// names a missing audit repository (it cannot append its entry without one).
func TestGetDigest_UnconfiguredIs501(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	w := serveDigest(t, s, http.MethodGet, "/v0/digest?repo=o/r", "", digestTokenIdentity("cap", scopeDigestRead))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501:\n%s", w.Code, w.Body.String())
	}
	e := digestErrorBody(t, w)
	if e.Code != "digest_unconfigured" || !strings.Contains(e.Message, "digest_store") || !strings.Contains(e.Message, "decision_index") {
		t.Errorf("error = %+v, want digest_unconfigured naming digest_store and decision_index", e)
	}
}

func TestMarkRead_UnconfiguredIs501(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", DigestStore: digest.NewStore(nil), DigestIndex: digestNoIndex{t}})
	w := serveDigest(t, s, http.MethodPost, "/v0/digest/mark-read", `{"repo":"o/r","to_sequence":1}`,
		digestTokenIdentity("cap", scopeDigestMarkRead))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501:\n%s", w.Code, w.Body.String())
	}
	if e := digestErrorBody(t, w); e.Code != "digest_unconfigured" || !strings.Contains(e.Message, "audit_repository") {
		t.Errorf("error = %+v, want digest_unconfigured naming audit_repository", e)
	}
}

// TestGetDigest_AnonymousIs401: the digest is per-captain; no subject, no
// digest.
func TestGetDigest_AnonymousIs401(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", DigestStore: digest.NewStore(nil), DigestIndex: digestNoIndex{t}})
	w := serveDigest(t, s, http.MethodGet, "/v0/digest?repo=o/r", "", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

// TestGetDigest_RequestShapeRefused: each query-parameter guard refuses with a
// 400 naming ITS field, before any read. The field detail is what isolates the
// handler guard from digest.Build's own validation (which answers 400 with no
// field), so deleting a handler guard reddens its row.
func TestGetDigest_RequestShapeRefused(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", DigestStore: digest.NewStore(nil), DigestIndex: digestNoIndex{t}})
	for _, c := range []struct{ name, query, field string }{
		{"missing repo", "", "repo"},
		{"unknown section", "repo=o/r&section=doctrine_changes", "section"},
		{"negative from", "repo=o/r&from_sequence=-1", "from_sequence"},
		{"non-integer to", "repo=o/r&to_sequence=abc", "to_sequence"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := serveDigest(t, s, http.MethodGet, "/v0/digest?"+c.query, "", digestTokenIdentity("cap", scopeDigestRead))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			if e := digestErrorBody(t, w); e.Code != "validation_failed" || e.Details["field"] != c.field {
				t.Errorf("error = %+v, want validation_failed field=%s", e, c.field)
			}
		})
	}
}

// TestMarkRead_RequestShapeRefused: the body guards refuse with a 400 before
// the store is reached (the nil-DB store would panic if it were).
func TestMarkRead_RequestShapeRefused(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", DigestStore: digest.NewStore(nil), DigestIndex: digestNoIndex{t},
		AuditRepo: newAuditFake()})
	for _, c := range []struct{ name, body, field string }{
		{"missing repo", `{"to_sequence":5}`, "repo"},
		{"zero to_sequence", `{"repo":"o/r","to_sequence":0}`, "to_sequence"},
		{"negative to_sequence", `{"repo":"o/r","to_sequence":-3}`, "to_sequence"},
		{"unknown field", `{"repo":"o/r","to_sequence":5,"force":true}`, ""},
		{"not json", `nope`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := serveDigest(t, s, http.MethodPost, "/v0/digest/mark-read", c.body, digestTokenIdentity("cap", scopeDigestMarkRead))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
			}
			e := digestErrorBody(t, w)
			if e.Code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", e.Code)
			}
			if c.field != "" && e.Details["field"] != c.field {
				t.Errorf("details = %v, want field=%s", e.Details, c.field)
			}
		})
	}
}

// TestOpenAPI_DigestRoutesDocumented: both routes are in the OpenAPI source of
// truth, with the scopes the handlers enforce.
func TestOpenAPI_DigestRoutesDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{"\n  /v0/digest:\n", "\n  /v0/digest/mark-read:\n", "operationId: getDigest", "operationId: markDigestRead"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}
