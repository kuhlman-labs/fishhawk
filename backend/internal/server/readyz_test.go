package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readyz_test.go pins GET /readyz (#5224 G7): the readiness probe's
// responses, its bounded and detached shared ping, its coalescing and its
// cache. Every arm drives a fake Config.DBPing; the real-pool path is
// TestServe_Readyz_PingsConfiguredDatabase in cmd/fishhawkd.

// rzSentinelHost stands in for the database host a pgx dial error names.
// Neither the response body nor the log line may carry it.
const rzSentinelHost = "rz-sentinel-db-7f3a.internal"

// rzLogBuf is a race-safe log sink: the shared ping logs from its own
// goroutine.
type rzLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *rzLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *rzLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func rzServer(t *testing.T, ping func(context.Context) error) (*Server, *rzLogBuf) {
	t.Helper()
	logs := &rzLogBuf{}
	s := New(Config{DBPing: ping, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	return s, logs
}

func rzServe(h http.HandlerFunc, ctx context.Context) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx))
	return rec
}

// rzWant asserts the status code and the decoded body's two fields.
func rzWant(t *testing.T, rec *httptest.ResponseRecorder, code int, status, database string) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status code = %d, want %d; body %s", rec.Code, code, rec.Body.String())
	}
	var body struct {
		Status string `json:"status"`
		Checks struct {
			Database string `json:"database"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /readyz body: %v\n%s", err, rec.Body.String())
	}
	if body.Status != status || body.Checks.Database != database {
		t.Fatalf("body = {status:%q checks.database:%q}, want {%q %q}: %s",
			body.Status, body.Checks.Database, status, database, rec.Body.String())
	}
}

// RZ1: a successful ping answers 200 {status ok, checks.database ok} as
// uncacheable JSON.
func TestReadyz_PingOK_Answers200(t *testing.T) {
	s, _ := rzServer(t, func(context.Context) error { return nil })
	rec := rzServe(s.handleReady, context.Background())
	rzWant(t, rec, http.StatusOK, "ok", "ok")
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
}

// RZ2: a failing ping answers 503 unreachable. The driver error names the
// database host; neither the body nor the log line may carry it, and the
// log line records the error class instead.
func TestReadyz_PingError_Answers503WithoutEchoingTheError(t *testing.T) {
	s, logs := rzServer(t, func(context.Context) error {
		return fmt.Errorf("failed to connect to `host=%s user=fishhawk`: dial tcp: connection refused", rzSentinelHost)
	})
	rec := rzServe(s.handleReady, context.Background())
	rzWant(t, rec, http.StatusServiceUnavailable, "unavailable", "unreachable")
	if strings.Contains(rec.Body.String(), rzSentinelHost) {
		t.Errorf("/readyz body echoes the driver error: %s", rec.Body.String())
	}
	got := logs.String()
	if strings.Contains(got, rzSentinelHost) {
		t.Errorf("ping-failure log line carries the raw driver error:\n%s", got)
	}
	if !strings.Contains(got, "class=unreachable") {
		t.Errorf("ping-failure log line does not record the error class:\n%s", got)
	}
}

// RZ3: no database configured answers 503 not_configured, logged once per
// process rather than once per probe. A Server built without New (nil probe)
// answers the same.
func TestReadyz_NoDatabase_Answers503NotConfigured(t *testing.T) {
	s, logs := rzServer(t, nil)
	for i := 0; i < 2; i++ {
		rzWant(t, rzServe(s.handleReady, context.Background()), http.StatusServiceUnavailable, "unavailable", "not_configured")
	}
	if n := strings.Count(logs.String(), "class=not_configured"); n != 1 {
		t.Errorf("not_configured logged %d times across two probes, want once:\n%s", n, logs.String())
	}
	rzWant(t, rzServe((&Server{}).handleReady, context.Background()), http.StatusServiceUnavailable, "unavailable", "not_configured")
}

// RZ4: a ping that never returns on its own is bounded by the probe's
// timeout. Without the bound the request would hang until the 10s guard.
func TestReadyz_HungPing_BoundedByTimeout(t *testing.T) {
	var hadDeadline atomic.Bool
	s, logs := rzServer(t, func(ctx context.Context) error {
		_, ok := ctx.Deadline()
		hadDeadline.Store(ok)
		<-ctx.Done()
		return ctx.Err()
	})
	s.readiness.timeout = 50 * time.Millisecond
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- rzServe(s.handleReady, context.Background()) }()
	select {
	case rec := <-done:
		rzWant(t, rec, http.StatusServiceUnavailable, "unavailable", "unreachable")
	case <-time.After(10 * time.Second):
		t.Fatal("/readyz did not answer within 10s: the shared ping is unbounded")
	}
	if !hadDeadline.Load() {
		t.Error("the ping's context carried no deadline")
	}
	if !strings.Contains(logs.String(), "class=timeout") {
		t.Errorf("a timed-out ping is not logged as class=timeout:\n%s", logs.String())
	}
}

// RZ4b: the shared ping is detached from the request that started it. The
// first caller disconnects mid-ping and gets its 503 at once; the ping is not
// cancelled, so a second caller waiting on the same ping still gets 200.
func TestReadyz_CallerDisconnect_DoesNotCancelSharedPing(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s, _ := rzServer(t, func(ctx context.Context) error {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	ctxA, cancelA := context.WithCancel(context.Background())
	recA := make(chan *httptest.ResponseRecorder, 1)
	go func() { recA <- rzServe(s.handleReady, ctxA) }()
	<-entered
	recB := make(chan *httptest.ResponseRecorder, 1)
	go func() { recB <- rzServe(s.handleReady, context.Background()) }()
	cancelA()
	// Well inside the 2s ping bound: A must return on its own context, not
	// on the shared ping's deadline.
	select {
	case rec := <-recA:
		rzWant(t, rec, http.StatusServiceUnavailable, "unavailable", "unreachable")
	case <-time.After(time.Second):
		t.Fatal("a disconnected caller kept waiting on the shared ping")
	}
	// Let B join the in-flight ping before it completes.
	time.Sleep(50 * time.Millisecond)
	close(release)
	select {
	case rec := <-recB:
		rzWant(t, rec, http.StatusOK, "ok", "ok")
	case <-time.After(10 * time.Second):
		t.Fatal("the second caller never got the shared ping's result")
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("DBPing calls = %d, want 1 (B joins the in-flight ping)", n)
	}
}

// RZ5: concurrent anonymous probes share one ping. Without the coalescer
// every request would call DBPing, giving 64 calls and 64 pool connections.
func TestReadyz_ConcurrentProbes_CoalesceToOnePing(t *testing.T) {
	const n = 64
	release := make(chan struct{})
	var calls atomic.Int32
	s, _ := rzServer(t, func(context.Context) error {
		calls.Add(1)
		<-release
		return nil
	})
	h := s.Handler()
	var started, finished sync.WaitGroup
	codes := make([]int, n)
	started.Add(n)
	finished.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer finished.Done()
			started.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			codes[i] = rec.Code
		}(i)
	}
	started.Wait()
	time.Sleep(100 * time.Millisecond)
	close(release)
	finished.Wait()
	if got := calls.Load(); got > 2 {
		t.Errorf("DBPing calls = %d for %d concurrent probes, want <= 2 (expected 1)", got, n)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("probe %d answered %d, want 200", i, c)
		}
	}
}

// RZ6: a result answers every probe for the TTL, success and failure alike;
// a probe after the TTL pings again.
func TestReadyz_ResultCachedForTTL(t *testing.T) {
	var mu sync.Mutex
	clock := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }
	var calls atomic.Int32
	var fail atomic.Bool
	s, _ := rzServer(t, func(context.Context) error {
		calls.Add(1)
		if fail.Load() {
			return errors.New("connection refused")
		}
		return nil
	})
	s.nowFunc = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }

	rzWant(t, rzServe(s.handleReady, context.Background()), http.StatusOK, "ok", "ok")
	advance(500 * time.Millisecond)
	rzWant(t, rzServe(s.handleReady, context.Background()), http.StatusOK, "ok", "ok")
	if got := calls.Load(); got != 1 {
		t.Fatalf("DBPing calls within the TTL = %d, want 1", got)
	}
	advance(1100 * time.Millisecond)
	fail.Store(true)
	rzWant(t, rzServe(s.handleReady, context.Background()), http.StatusServiceUnavailable, "unavailable", "unreachable")
	if got := calls.Load(); got != 2 {
		t.Fatalf("DBPing calls after the TTL = %d, want 2", got)
	}
	// The failure is cached too: the database recovering inside the TTL is
	// not observed until the TTL lapses.
	fail.Store(false)
	advance(500 * time.Millisecond)
	rzWant(t, rzServe(s.handleReady, context.Background()), http.StatusServiceUnavailable, "unavailable", "unreachable")
	if got := calls.Load(); got != 2 {
		t.Fatalf("DBPing calls for a cached failure = %d, want 2", got)
	}
	advance(600 * time.Millisecond)
	rzWant(t, rzServe(s.handleReady, context.Background()), http.StatusOK, "ok", "ok")
	if got := calls.Load(); got != 3 {
		t.Fatalf("DBPing calls after the failure's TTL = %d, want 3", got)
	}
}

// RZ7: through the real router, /readyz is public (an anonymous probe gets
// 200), and /healthz stays liveness-only (200 even while the ping fails).
func TestReadyz_RouterAnonymous_AndHealthzUnchanged(t *testing.T) {
	ok, _ := rzServer(t, func(context.Context) error { return nil })
	rec := httptest.NewRecorder()
	ok.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	rzWant(t, rec, http.StatusOK, "ok", "ok")

	down, _ := rzServer(t, func(context.Context) error { return errors.New("connection refused") })
	rec = httptest.NewRecorder()
	down.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d with a failing database ping, want 200 (liveness only): %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	down.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	rzWant(t, rec, http.StatusServiceUnavailable, "unavailable", "unreachable")
}

// RZ8: the route, its operation id and its closed vocabulary are in the
// OpenAPI source of truth.
func TestOpenAPI_ReadyzRouteDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"\n  /readyz:\n", "operationId: getReadyz",
		"\n    ReadinessResponse:\n", "enum: [ok, unavailable]",
		"enum: [ok, unreachable, not_configured]",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}
