package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestID_GeneratesWhenAbsent(t *testing.T) {
	var captured string
	h := requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if captured == "" {
		t.Fatal("expected a generated request ID on the context")
	}
	if got := rec.Header().Get("X-Request-ID"); got != captured {
		t.Errorf("X-Request-ID header = %q, want %q", got, captured)
	}
}

func TestRequestID_HonorsClientID(t *testing.T) {
	const supplied = "trace-1234"
	var captured string
	h := requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", supplied)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured != supplied {
		t.Errorf("captured = %q, want %q", captured, supplied)
	}
}

func TestRequestID_RejectsOversizedClientID(t *testing.T) {
	huge := strings.Repeat("a", requestIDMaxLen+1)
	var captured string
	h := requestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = RequestIDFrom(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", huge)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if captured == huge {
		t.Error("oversized client ID should have been replaced with a generated one")
	}
	if captured == "" {
		t.Error("expected a generated request ID after rejection")
	}
}

func TestRecovery_CatchesPanicAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := recovery(logger)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	logs := buf.String()
	if !strings.Contains(logs, `"msg":"panic"`) {
		t.Errorf("log missing panic event:\n%s", logs)
	}
	if !strings.Contains(logs, "boom") {
		t.Errorf("log missing recovered value:\n%s", logs)
	}
}

func TestBearerAuth_NoHeader_Anonymous(t *testing.T) {
	var captured Identity
	h := bearerAuth(nil, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = IdentityFrom(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !captured.IsAnonymous() {
		t.Errorf("identity = %+v, want anonymous", captured)
	}
}

func TestLogging_EmitsStructuredEvent(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	// Wrap with requestID first so the log line carries one.
	h := requestID(logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "short and stout")
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/foo", nil))

	out := buf.String()
	for _, want := range []string{
		`"msg":"request"`,
		`"method":"GET"`,
		`"path":"/foo"`,
		`"status":418`,
		`"request_id":`,
		`"duration_ms":`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %s:\n%s", want, out)
		}
	}
}
