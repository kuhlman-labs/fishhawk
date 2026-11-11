package pgtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/postgres"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// --- in-process fake servers (no Docker) ---

// fakeServer accepts on 127.0.0.1:0 and runs handle per connection. It closes
// the listener and every accepted connection when the test's deadline ctx
// ends, so a stall fake cannot wedge a test instead of failing it, and again
// at t.Cleanup.
type fakeServer struct {
	ln     net.Listener
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func startFake(t *testing.T, deadline context.Context, handle func(net.Conn)) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeServer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			if f.closed {
				f.mu.Unlock()
				_ = c.Close()
				return
			}
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go func() {
				defer func() { _ = c.Close() }()
				handle(c)
			}()
		}
	}()
	stop := context.AfterFunc(deadline, f.close)
	t.Cleanup(func() {
		stop()
		f.close()
	})
	return f
}

func (f *fakeServer) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	_ = f.ln.Close()
	for _, c := range f.conns {
		_ = c.Close()
	}
}

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

func (f *fakeServer) url(user, pass, db string) string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", user, pass, f.addr(), db)
}

// refusedAddr is a 127.0.0.1 port nothing listens on.
func refusedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// stallBeforeStartup accepts and reads but never writes a byte.
func stallBeforeStartup(c net.Conn) { _, _ = io.Copy(io.Discard, c) }

type backendMode int

const (
	healthy backendMode = iota
	stallBeforeReady
	stallOnQuery
	refuse53300
	eofAfterAccept
)

// pgBackend is a pgproto3 fake server parameterized by mode.
func pgBackend(mode backendMode) func(net.Conn) {
	return func(c net.Conn) {
		b := pgproto3.NewBackend(c, c)
		msg, err := b.ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := msg.(*pgproto3.SSLRequest); ok {
			if _, err := c.Write([]byte("N")); err != nil {
				return
			}
			if _, err := b.ReceiveStartupMessage(); err != nil {
				return
			}
		}
		switch mode {
		case eofAfterAccept:
			return
		case refuse53300:
			b.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "53300", Message: "sorry, too many clients already"})
			_ = b.Flush()
			return
		case stallBeforeReady:
			b.Send(&pgproto3.AuthenticationOk{})
			_ = b.Flush()
			_, _ = io.Copy(io.Discard, c)
			return
		}
		b.Send(&pgproto3.AuthenticationOk{})
		for _, p := range [][2]string{
			{"server_version", "16.0"}, {"client_encoding", "UTF8"}, {"standard_conforming_strings", "on"},
			{"DateStyle", "ISO, MDY"}, {"integer_datetimes", "on"},
		} {
			b.Send(&pgproto3.ParameterStatus{Name: p[0], Value: p[1]})
		}
		b.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 1}})
		b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := b.Flush(); err != nil {
			return
		}
		for {
			msg, err := b.Receive()
			if err != nil {
				return
			}
			switch m := msg.(type) {
			case *pgproto3.Terminate:
				return
			case *pgproto3.Query:
				if mode == stallOnQuery {
					continue // never answer
				}
				q := strings.TrimSpace(m.String)
				if q == "" || strings.HasPrefix(q, "--") {
					b.Send(&pgproto3.EmptyQueryResponse{})
				} else {
					b.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 0")})
				}
				b.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if err := b.Flush(); err != nil {
					return
				}
			}
		}
	}
}

// testDeadline is the test's own timescale.D deadline ctx: every fake closes
// its connections when it ends.
func testDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timescale.D(5*time.Second))
	t.Cleanup(cancel)
	return ctx
}

// shortCtx is the connect/probe deadline, well inside testDeadline.
func shortCtx(t *testing.T, parent context.Context) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, timescale.D(400*time.Millisecond))
	t.Cleanup(cancel)
	return ctx
}

func blockUntilDone(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// --- error shape -> phase, against the PRODUCTION postgres.Connect ---

// TestClassifyConnectErr_RealPgxShapes pins, against the pinned pgx and
// puddle, that a server which accepts and never answers produces EXACTLY the
// observed production string `ping: context deadline exceeded` classified
// acquire-wait (a new connection not built in the window), while a stalled
// query on an established connection is query-timeout.
func TestClassifyConnectErr_RealPgxShapes(t *testing.T) {
	type row struct {
		name    string
		handle  func(net.Conn)
		refused bool
		want    string
		wantMsg string
	}
	rows := []row{
		{name: "accept and stall", handle: stallBeforeStartup, want: "acquire-wait", wantMsg: "ping: context deadline exceeded"},
		{name: "stall on established query", handle: pgBackend(stallOnQuery), want: "query-timeout"},
		{name: "fatal 53300", handle: pgBackend(refuse53300), want: "connect-error:sqlstate=53300"},
		{name: "refused port", refused: true, want: "connect-error:refused"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			deadline := testDeadline(t)
			var addr string
			if r.refused {
				addr = refusedAddr(t)
			} else {
				addr = startFake(t, deadline, r.handle).addr()
			}
			pool, err := postgres.Connect(shortCtx(t, deadline), "postgres://u:p@"+addr+"/d?sslmode=disable")
			if pool != nil {
				pool.Close()
			}
			if err == nil {
				t.Fatal("postgres.Connect succeeded against a failing fake")
			}
			if got := classifyConnectErr(err); got != r.want {
				t.Errorf("classifyConnectErr(%q) = %q, want %q", err, got, r.want)
			}
			if r.wantMsg != "" && err.Error() != r.wantMsg {
				t.Errorf("err.Error() = %q, want exactly %q", err.Error(), r.wantMsg)
			}
		})
	}

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "ok"},
		{"wrapped bare deadline", fmt.Errorf("ping: %w", context.DeadlineExceeded), "acquire-wait"},
		{"bare server error", fmt.Errorf("x: %w", &pgconn.PgError{Code: "57P03"}), "server-error:sqlstate=57P03"},
		{"other", errors.New("boom"), "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyConnectErr(tc.err); got != tc.want {
				t.Errorf("classifyConnectErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// --- phase attribution, one row per diagnosed phase ---

func TestProbe_PhaseAttribution(t *testing.T) {
	type row struct {
		name    string
		handle  func(net.Conn)
		refused bool
		hooks   probeHooks
		want    string
	}
	rows := []row{
		{name: "lookup stall", handle: pgBackend(healthy), want: "stalled-at-lookup",
			hooks: probeHooks{lookup: func(ctx context.Context, _ string) ([]string, error) { return nil, blockUntilDone(ctx) }}},
		{name: "dial stall", handle: pgBackend(healthy), want: "stalled-at-dial",
			hooks: probeHooks{dial: func(ctx context.Context, _, _ string) (net.Conn, error) { return nil, blockUntilDone(ctx) }}},
		{name: "awaiting server", handle: stallBeforeStartup, want: "stalled-awaiting-server"},
		{name: "in startup", handle: pgBackend(stallBeforeReady), want: "stalled-in-startup"},
		{name: "at ping", handle: pgBackend(stallOnQuery), want: "stalled-at-ping"},
		{name: "fatal 53300", handle: pgBackend(refuse53300), want: "connect-error:sqlstate=53300"},
		{name: "refused", refused: true, want: "connect-error:refused"},
		{name: "eof after accept", handle: pgBackend(eofAfterAccept), want: "connect-error:error"},
		{name: "healthy", handle: pgBackend(healthy), want: "ok"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			deadline := testDeadline(t)
			var addr string
			if r.refused {
				addr = refusedAddr(t)
			} else {
				addr = startFake(t, deadline, r.handle).addr()
			}
			var readyCalls atomic.Int32
			got := probeTarget(shortCtx(t, deadline), "postgres://u:p@"+addr+"/d?sslmode=disable", r.hooks,
				func(_ context.Context, c *pgconn.PgConn) {
					if c != nil {
						readyCalls.Add(1)
					}
				})
			if got.outcome != r.want {
				t.Fatalf("outcome = %q, want %q (result %+v)", got.outcome, r.want, got)
			}
			if r.want != "ok" {
				if n := readyCalls.Load(); n != 0 {
					t.Errorf("onReady called %d times on a failed probe, want 0", n)
				}
				return
			}
			// The healthy positive: every phase recorded, onReady once.
			if got.firstByte <= 0 || got.ready <= 0 || got.ping <= 0 {
				t.Errorf("healthy probe phases first-byte=%v ready=%v ping=%v, want all > 0", got.firstByte, got.ready, got.ping)
			}
			if got.firstByte > got.ready {
				t.Errorf("first-byte %v after ready %v", got.firstByte, got.ready)
			}
			if got.dials != 1 || got.addrs != 1 || strings.Join(got.families, ",") != "ipv4" {
				t.Errorf("dials=%d addrs=%d families=%v, want 1, 1, [ipv4]", got.dials, got.addrs, got.families)
			}
			if n := readyCalls.Load(); n != 1 {
				t.Errorf("onReady called %d times on the healthy probe, want 1", n)
			}
		})
	}
}

// --- secrecy ---

const (
	secretUser = "u_secret_7f3"
	secretPass = "pw_secret_9c1"
	secretDB   = "db_secret_4ab"
)

// assertNoSecrets fails when s carries any credential-shaped substring.
func assertNoSecrets(t *testing.T, where, s string, tokens ...string) {
	t.Helper()
	for _, bad := range append([]string{"postgres://", "user=", "database=", "password"}, tokens...) {
		if bad != "" && strings.Contains(s, bad) {
			t.Errorf("%s leaks %q: %s", where, bad, s)
		}
	}
}

func TestDiagnostics_NeverRenderSecrets(t *testing.T) {
	type row struct {
		name    string
		handle  func(net.Conn)
		refused bool
	}
	rows := []row{
		{name: "awaiting server", handle: stallBeforeStartup},
		{name: "healthy", handle: pgBackend(healthy)},
		{name: "in startup", handle: pgBackend(stallBeforeReady)},
		{name: "at ping", handle: pgBackend(stallOnQuery)},
		{name: "fatal 53300", handle: pgBackend(refuse53300)},
		{name: "eof after accept", handle: pgBackend(eofAfterAccept)},
		{name: "refused", refused: true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			deadline := testDeadline(t)
			var addr string
			if r.refused {
				addr = refusedAddr(t)
			} else {
				addr = startFake(t, deadline, r.handle).addr()
			}
			dsn := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", secretUser, secretPass, addr, secretDB)
			forbidden := []string{secretUser, secretPass, secretDB, addr}

			pool, err := postgres.Connect(shortCtx(t, deadline), dsn)
			if pool != nil {
				pool.Close()
			}
			shape := classifyConnectErr(err)
			assertNoSecrets(t, "classifyConnectErr", shape, forbidden...)

			res := probeTarget(shortCtx(t, deadline), dsn, probeHooks{}, nil)
			assertNoSecrets(t, "probe outcome", res.outcome, forbidden...)
			rep := diagReport{when: "live", perTest: res, base: res, server: &serverSnapshot{checkpointsReq: "n/a", checkpointsTimed: "n/a"}}
			assertNoSecrets(t, "renderDiag", renderDiag(shape, cloneTiming{}, time.Second, &rep), forbidden...)
		})
	}

	// The full Fatalf message keeps the original error as its prefix (which
	// may name user= and database=, as before) but never the password.
	t.Run("fatalf message", func(t *testing.T) {
		deadline := testDeadline(t)
		f := startFake(t, deadline, pgBackend(eofAfterAccept))
		dsn := f.url(secretUser, secretPass, secretDB)
		tb := &fakePoolTB{}
		cctx := shortCtx(t, deadline)
		newPoolWith(tb, poolOpen{
			url: dsn, baseURL: dsn, perTestDB: secretDB,
			connect: func(_ context.Context, u string) (*pgxpool.Pool, error) {
				return postgres.Connect(cctx, u)
			},
			probe: func(ctx context.Context, b, p, db, when string) diagReport {
				return runConnectProbe(ctx, b, p, db, when, probeHooks{})
			},
			after: neverAfter,
		})
		msg := tb.fatalMsg()
		if msg == "" {
			t.Fatal("newPoolWith did not Fatalf on a failing connect")
		}
		if strings.Contains(msg, secretPass) {
			t.Errorf("Fatalf message leaks the password: %s", msg)
		}
		_, suffix, ok := strings.Cut(msg, "; pgtest diagnostics:")
		if !ok {
			t.Fatalf("Fatalf message has no diagnostics suffix: %s", msg)
		}
		assertNoSecrets(t, "diagnostics suffix", suffix, secretUser, secretPass, secretDB, f.addr())
	})
}

// --- bounds ---

// runnerInfraMarkers are the runner's infrastructure-flake markers; the
// suffix must never contain one, so it cannot change failure classification.
var runnerInfraMarkers = []string{"docker.sock", "failed to start container", "mapped port", "wait until ready", "signal: "}

func fullReport(waits []waitCount) *diagReport {
	p := probeResult{target: "per-test", lookup: time.Millisecond, addrs: 2, families: []string{"ipv4", "ipv6"},
		dials: 2, lastDial: time.Millisecond, firstByte: 3 * time.Millisecond, ready: 4 * time.Millisecond,
		ping: time.Millisecond, outcome: "stalled-in-startup", readyLimit: 3 * time.Second}
	b := p
	b.target, b.outcome = "base", "ok"
	return &diagReport{when: "live", perTest: p, base: b, server: &serverSnapshot{
		maxConnections: 100, backends: 50, clientBackends: 40, perTestBackends: 1, fhDatabases: 30,
		templateDatabases: 2, uptimeSeconds: 3600, checkpointsReq: "12", checkpointsTimed: "3",
		checkpointerWait: "Activity:CheckpointerMain", waits: waits,
	}}
}

func TestRenderDiag_Bounded(t *testing.T) {
	long := strings.Repeat("x", 300)
	var waits []waitCount
	for i := range 50 {
		waits = append(waits, waitCount{backendType: "client backend " + long, event: fmt.Sprintf("IO:DataFileRead%d", i), n: int64(i)})
	}
	clone := cloneTiming{baseConnect: time.Millisecond, create: 40 * time.Millisecond, contentionRetries: 1}

	got := renderDiag("acquire-wait", clone, 5*time.Second, fullReport(waits))
	if len(got) > 1536 {
		t.Errorf("len = %d, want <= 1536", len(got))
	}
	if !strings.HasPrefix(got, "pgtest diagnostics: shape=acquire-wait ") {
		t.Errorf("unexpected prefix: %s", got)
	}
	_, after, ok := strings.Cut(got, "waits[")
	if !ok {
		t.Fatalf("no waits[ segment: %s", got)
	}
	inner, _, _ := strings.Cut(after, "]")
	if n := len(strings.Split(inner, "|")); n > 5 {
		t.Errorf("rendered %d wait entries, want <= 5", n)
	}
	for _, m := range append(runnerInfraMarkers, "fishhawk") {
		if strings.Contains(got, m) {
			t.Errorf("rendered line contains %q: %s", m, got)
		}
	}

	// A line over the cap is cut and marked.
	cut := renderDiag(strings.Repeat("s", 4000), clone, time.Second, fullReport(waits))
	if len(cut) > 1536 {
		t.Errorf("truncated len = %d, want <= 1536", len(cut))
	}
	if !strings.HasSuffix(cut, "...(truncated)") {
		t.Errorf("over-cap line lacks the truncation marker: ...%s", cut[len(cut)-40:])
	}
	if strings.Contains(got, "...(truncated)") {
		t.Errorf("under-cap line carries the truncation marker")
	}
}

// --- newPoolWith: fail-closed routing and probe triggering ---

// fakePoolTB records every terminal call; Fatalf returns, as with fataler.
type fakePoolTB struct {
	mu       sync.Mutex
	skips    int
	fatals   []string
	logs     []string
	cleanups []func()
	onFatal  func()
	fatalCh  chan struct{}
}

func (f *fakePoolTB) Helper() {}
func (f *fakePoolTB) Skipf(string, ...any) {
	f.mu.Lock()
	f.skips++
	f.mu.Unlock()
}
func (f *fakePoolTB) Fatalf(format string, args ...any) {
	if f.onFatal != nil {
		f.onFatal()
	}
	f.mu.Lock()
	f.fatals = append(f.fatals, fmt.Sprintf(format, args...))
	f.mu.Unlock()
	if f.fatalCh != nil {
		close(f.fatalCh)
	}
}
func (f *fakePoolTB) Logf(format string, args ...any) {
	f.mu.Lock()
	f.logs = append(f.logs, fmt.Sprintf(format, args...))
	f.mu.Unlock()
}
func (f *fakePoolTB) Cleanup(fn func()) {
	f.mu.Lock()
	f.cleanups = append(f.cleanups, fn)
	f.mu.Unlock()
}
func (f *fakePoolTB) fatalMsg() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.fatals) == 0 {
		return ""
	}
	return f.fatals[0]
}
func (f *fakePoolTB) runCleanups() {
	f.mu.Lock()
	fns := f.cleanups
	f.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

func neverAfter(time.Duration) <-chan time.Time { return make(chan time.Time) }

// lazyPool is a pool that never dials (MinConns 0, never acquired).
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), "postgres://u:p@"+refusedAddr(t)+"/d?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	return p
}

type probeCall struct {
	when        string
	hasDeadline bool
}

type probeRecorder struct {
	mu    sync.Mutex
	calls []probeCall
}

func (p *probeRecorder) record(ctx context.Context, when string) {
	_, ok := ctx.Deadline()
	p.mu.Lock()
	p.calls = append(p.calls, probeCall{when, ok})
	p.mu.Unlock()
}

func (p *probeRecorder) snapshot() []probeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]probeCall(nil), p.calls...)
}

func TestNewPoolWith_ConnectFailureFatalsNeverSkips(t *testing.T) {
	boom := errors.New("ping: context deadline exceeded")
	tb := &fakePoolTB{}
	rec := &probeRecorder{}
	got := newPoolWith(tb, poolOpen{
		url: "postgres://u:p@h:1/d", baseURL: "postgres://u:p@h:1/b", perTestDB: "d",
		connect: func(context.Context, string) (*pgxpool.Pool, error) { return nil, boom },
		probe: func(ctx context.Context, _, _, _, when string) diagReport {
			rec.record(ctx, when)
			return diagReport{when: when}
		},
		after: neverAfter,
	})
	if got != nil {
		t.Errorf("newPoolWith returned %v on a failed connect, want nil", got)
	}
	if tb.skips != 0 {
		t.Errorf("Skipf called %d times on a connect failure, want 0 (never skip)", tb.skips)
	}
	if len(tb.fatals) != 1 {
		t.Fatalf("Fatalf called %d times, want exactly 1", len(tb.fatals))
	}
	if want := "connect per-test pool: " + boom.Error() + "; pgtest diagnostics:"; !strings.HasPrefix(tb.fatals[0], want) {
		t.Errorf("Fatalf message = %q, want prefix %q", tb.fatals[0], want)
	}
	if !strings.Contains(tb.fatals[0], "shape=other") {
		t.Errorf("Fatalf message lacks the classified shape: %s", tb.fatals[0])
	}
	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].when != "post-failure" || !calls[0].hasDeadline {
		t.Errorf("probe calls = %+v, want exactly one bounded post-failure probe", calls)
	}
	if len(tb.cleanups) != 0 {
		t.Errorf("Cleanup registered %d times on a failed connect, want 0", len(tb.cleanups))
	}
}

func TestNewPoolWith_StallTriggersOneLiveProbe(t *testing.T) {
	fire := make(chan time.Time, 1)
	connectRelease := make(chan struct{})
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	var probeReturned atomic.Bool
	var returnedAtFatal atomic.Bool
	rec := &probeRecorder{}
	tb := &fakePoolTB{fatalCh: make(chan struct{})}
	tb.onFatal = func() { returnedAtFatal.Store(probeReturned.Load()) }

	done := make(chan *pgxpool.Pool, 1)
	go func() {
		done <- newPoolWith(tb, poolOpen{
			url: "postgres://u:p@h:1/d", baseURL: "postgres://u:p@h:1/b", perTestDB: "d",
			connect: func(context.Context, string) (*pgxpool.Pool, error) {
				<-connectRelease
				return nil, context.DeadlineExceeded
			},
			probe: func(ctx context.Context, _, _, _, when string) diagReport {
				defer probeReturned.Store(true)
				rec.record(ctx, when)
				close(probeStarted)
				select {
				case <-probeRelease:
				case <-ctx.Done():
				}
				return diagReport{when: when}
			},
			after: func(time.Duration) <-chan time.Time { return fire },
		})
	}()

	fire <- time.Now()
	<-probeStarted
	close(connectRelease) // connect fails while the live probe is still running
	select {
	case <-tb.fatalCh: // Fatalf before the probe returned: the join is missing
	case <-time.After(timescale.D(100 * time.Millisecond)):
	}
	close(probeRelease)
	if got := <-done; got != nil {
		t.Errorf("newPoolWith returned %v on a failed connect, want nil", got)
	}

	calls := rec.snapshot()
	if len(calls) != 1 || calls[0].when != "live" || !calls[0].hasDeadline {
		t.Errorf("probe calls = %+v, want exactly one bounded live probe and no post-failure probe", calls)
	}
	if len(tb.fatals) != 1 || tb.skips != 0 {
		t.Fatalf("fatals=%d skips=%d, want 1 and 0", len(tb.fatals), tb.skips)
	}
	if !returnedAtFatal.Load() {
		t.Error("Fatalf recorded before the live probe returned; the probe must be joined first")
	}
	if !strings.Contains(tb.fatals[0], "shape=acquire-wait") || !strings.Contains(tb.fatals[0], "probe[live]") {
		t.Errorf("Fatalf message lacks the live report: %s", tb.fatals[0])
	}
}

func TestNewPoolWith_FastConnectNeverProbes(t *testing.T) {
	pool := lazyPool(t)
	tb := &fakePoolTB{}
	rec := &probeRecorder{}
	got := newPoolWith(tb, poolOpen{
		url: "postgres://u:p@h:1/d", baseURL: "postgres://u:p@h:1/b", perTestDB: "d",
		connect: func(context.Context, string) (*pgxpool.Pool, error) { return pool, nil },
		probe: func(ctx context.Context, _, _, _, when string) diagReport {
			rec.record(ctx, when)
			return diagReport{when: when}
		},
		after: neverAfter, // the watchdog never fires
	})
	defer tb.runCleanups()
	if got != pool {
		t.Errorf("newPoolWith returned %v, want the connected pool", got)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("probe calls = %+v on a fast connect, want none", calls)
	}
	if len(tb.logs) != 0 || len(tb.fatals) != 0 || tb.skips != 0 {
		t.Errorf("logs=%v fatals=%v skips=%d, want none", tb.logs, tb.fatals, tb.skips)
	}
	if len(tb.cleanups) != 1 {
		t.Errorf("Cleanup registered %d times, want 1 (pool.Close)", len(tb.cleanups))
	}
}

func TestNewPoolWith_SlowSuccessJoinsProbe(t *testing.T) {
	pool := lazyPool(t)
	fire := make(chan time.Time, 1)
	connectRelease := make(chan struct{})
	probeStarted := make(chan struct{})
	var probeCtxErr atomic.Value
	var probeReturned atomic.Bool
	rec := &probeRecorder{}
	tb := &fakePoolTB{}

	type out struct {
		pool          *pgxpool.Pool
		probeReturned bool
	}
	done := make(chan out, 1)
	go func() {
		p := newPoolWith(tb, poolOpen{
			url: "postgres://u:p@h:1/d", baseURL: "postgres://u:p@h:1/b", perTestDB: "d",
			connect: func(context.Context, string) (*pgxpool.Pool, error) {
				<-connectRelease
				return pool, nil
			},
			probe: func(ctx context.Context, _, _, _, when string) diagReport {
				defer probeReturned.Store(true)
				rec.record(ctx, when)
				close(probeStarted)
				<-ctx.Done()
				probeCtxErr.Store(ctx.Err())
				return diagReport{when: when}
			},
			after: func(time.Duration) <-chan time.Time { return fire },
		})
		done <- out{p, probeReturned.Load()}
	}()

	fire <- time.Now()
	<-probeStarted
	close(connectRelease)
	o := <-done
	defer tb.runCleanups()

	if o.pool != pool {
		t.Errorf("newPoolWith returned %v, want the connected pool", o.pool)
	}
	if !o.probeReturned {
		t.Error("newPoolWith returned before the live probe was joined")
	}
	if err, _ := probeCtxErr.Load().(error); !errors.Is(err, context.Canceled) {
		t.Errorf("probe ctx err = %v, want context.Canceled (cancelled on success)", err)
	}
	if calls := rec.snapshot(); len(calls) != 1 || calls[0].when != "live" {
		t.Errorf("probe calls = %+v, want exactly one live probe", calls)
	}
	if len(tb.fatals) != 0 || tb.skips != 0 {
		t.Errorf("fatals=%v skips=%d on a slow success, want none", tb.fatals, tb.skips)
	}
	if len(tb.logs) != 1 || !strings.Contains(tb.logs[0], "slow per-test pool connect") || !strings.Contains(tb.logs[0], "pgtest diagnostics: shape=ok") {
		t.Errorf("logs = %v, want one slow-connect report", tb.logs)
	}
	if len(tb.cleanups) != 1 {
		t.Errorf("Cleanup registered %d times, want 1 (pool.Close)", len(tb.cleanups))
	}
}
