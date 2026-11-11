package pgtest

// Connect diagnostics for the per-test pool helper (#5116). See the
// "Connect diagnostics" paragraph of the package doc for the contract; this
// file holds the pure classifier, the phase-timed probe, the read-only server
// snapshot and the bounded renderer. Nothing here may render a DSN, a user,
// a password, a database name, a host:port or query text.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// maxDiagBytes caps the rendered diagnostics line, truncation marker
// included.
const maxDiagBytes = 1536

// maxWaitEntries caps the wait-event histogram both in the snapshot query and
// in the rendered line.
const maxWaitEntries = 5

const truncMarker = "...(truncated)"

// cloneTiming is how long the per-test database clone took before the pool
// connect started.
type cloneTiming struct {
	baseConnect       time.Duration
	create            time.Duration
	contentionRetries int
}

// classifyConnectErr maps a postgres.Connect error to a credential-free
// shape. The order matters: a ConnectError or PgError is checked before the
// timeout arms, and pgconn.Timeout (an established-connection query stall,
// rendered "timeout: ...") is checked before the bare DeadlineExceeded arm,
// because *errTimeout unwraps to context.DeadlineExceeded. A bare
// DeadlineExceeded from pgxpool.Ping is puddle's Acquire giving up on a
// connection constructor that is still running: a NEW connection was not
// built inside the window.
func classifyConnectErr(err error) string {
	if err == nil {
		return "ok"
	}
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return "connect-error:" + errClass(ce.Unwrap())
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return "server-error:sqlstate=" + sanitizeToken(pg.Code)
	}
	if pgconn.Timeout(err) {
		return "query-timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "acquire-wait"
	}
	return "other"
}

// errClass reduces an inner error to a fixed vocabulary. It never returns or
// embeds err.Error(): pgconn's per-dial errors carry host:port.
func errClass(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return "sqlstate=" + sanitizeToken(pg.Code)
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "refused"
	}
	if pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

// probeHooks are test-only overrides of the probe's address lookup and dial.
// The zero value uses pgconn's parsed defaults.
type probeHooks struct {
	lookup func(ctx context.Context, host string) ([]string, error)
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

// probeResult is the phase timing of one probe connection. Offsets are from
// the probe's start; a negative offset means the phase was never reached.
type probeResult struct {
	target     string // "per-test" or "base"
	lookup     time.Duration
	addrs      int
	families   []string // "ipv4", "ipv6", "unix"; never addresses
	dials      int
	lastDial   time.Duration
	firstByte  time.Duration
	ready      time.Duration
	ping       time.Duration
	outcome    string
	readyLimit time.Duration // the probe ctx budget, for the reader
}

// probeClock records phase timestamps as nanosecond offsets from start. The
// fields are atomic because pgconn reads the socket from its own goroutine.
type probeClock struct {
	start       time.Time
	lookupStart atomic.Int64
	lookupEnd   atomic.Int64
	dialStart   atomic.Int64
	dialEnd     atomic.Int64
	firstByte   atomic.Int64
	dials       atomic.Int32
	lastDial    atomic.Int64

	mu       sync.Mutex
	addrs    int
	families map[string]bool
}

func newProbeClock() *probeClock {
	c := &probeClock{start: time.Now(), families: map[string]bool{}}
	for _, a := range []*atomic.Int64{&c.lookupStart, &c.lookupEnd, &c.dialStart, &c.dialEnd, &c.firstByte} {
		a.Store(-1)
	}
	return c
}

func (c *probeClock) now() int64 { return int64(time.Since(c.start)) + 1 }

func (c *probeClock) addFamily(f string) {
	c.mu.Lock()
	c.families[f] = true
	c.mu.Unlock()
}

// timedConn records the offset of the first Read that returned bytes.
type timedConn struct {
	net.Conn
	clock *probeClock
}

func (tc *timedConn) Read(b []byte) (int, error) {
	n, err := tc.Conn.Read(b)
	if n > 0 {
		tc.clock.firstByte.CompareAndSwap(-1, tc.clock.now())
	}
	return n, err
}

// probeTarget opens one phase-timed connection to rawURL, pings it, invokes
// onReady (when non-nil) on the established connection, and closes it. On
// ctx expiry the outcome names the furthest phase reached. It returns when
// ctx ends: pgconn honours ctx in lookup, dial and every read.
func probeTarget(ctx context.Context, rawURL string, h probeHooks, onReady func(context.Context, *pgconn.PgConn)) (res probeResult) {
	res = probeResult{lookup: -1, lastDial: -1, firstByte: -1, ready: -1, ping: -1}
	if dl, ok := ctx.Deadline(); ok {
		res.readyLimit = time.Until(dl)
	}
	cfg, err := pgconn.ParseConfig(rawURL)
	if err != nil {
		res.outcome = "config-error"
		return res
	}
	clock := newProbeClock()
	lookup := cfg.LookupFunc
	if h.lookup != nil {
		lookup = h.lookup
	}
	dial := cfg.DialFunc
	if h.dial != nil {
		dial = h.dial
	}
	cfg.LookupFunc = func(ctx context.Context, host string) ([]string, error) {
		clock.lookupStart.CompareAndSwap(-1, clock.now())
		addrs, err := lookup(ctx, host)
		if err == nil {
			clock.lookupEnd.Store(clock.now())
			clock.mu.Lock()
			clock.addrs += len(addrs)
			clock.mu.Unlock()
			for _, a := range addrs {
				if ip := net.ParseIP(a); ip != nil && ip.To4() == nil {
					clock.addFamily("ipv6")
				} else {
					clock.addFamily("ipv4")
				}
			}
		}
		return addrs, err
	}
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network == "unix" {
			clock.addFamily("unix")
		}
		// Phase offsets describe the LATEST attempt: pgconn may try
		// several addresses in turn.
		at := clock.now()
		clock.dialStart.Store(at)
		clock.dialEnd.Store(-1)
		clock.firstByte.Store(-1)
		clock.dials.Add(1)
		conn, err := dial(ctx, network, addr)
		end := clock.now()
		clock.lastDial.Store(end - at)
		if err != nil {
			return nil, err
		}
		clock.dialEnd.Store(end)
		return &timedConn{Conn: conn, clock: clock}, nil
	}

	conn, connErr := pgconn.ConnectConfig(ctx, cfg)
	defer func() { res.fill(clock) }()
	if connErr != nil {
		res.outcome = connectOutcome(ctx, connErr, clock)
		return res
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	res.ready = time.Duration(clock.now())
	pingStart := time.Now()
	if err := conn.Ping(ctx); err != nil {
		if ctx.Err() != nil || pgconn.Timeout(err) {
			res.outcome = "stalled-at-ping"
		} else {
			res.outcome = "connect-error:" + errClass(err)
		}
		return res
	}
	res.ping = time.Since(pingStart)
	if onReady != nil {
		onReady(ctx, conn)
	}
	res.outcome = "ok"
	return res
}

// fill copies the clock's phase offsets into the result.
func (r *probeResult) fill(c *probeClock) {
	if s, e := c.lookupStart.Load(), c.lookupEnd.Load(); s >= 0 && e >= 0 {
		r.lookup = time.Duration(e - s)
	}
	if fb := c.firstByte.Load(); fb >= 0 {
		r.firstByte = time.Duration(fb)
	}
	r.dials = int(c.dials.Load())
	if r.dials > 0 {
		r.lastDial = time.Duration(c.lastDial.Load())
	}
	c.mu.Lock()
	r.addrs = c.addrs
	for f := range c.families {
		r.families = append(r.families, f)
	}
	c.mu.Unlock()
	sort.Strings(r.families)
}

// connectOutcome attributes a failed probe connect. A server error or a
// non-timeout failure is a connect-error; a ctx expiry names the furthest
// phase the attempt reached.
func connectOutcome(ctx context.Context, err error, c *probeClock) string {
	var pg *pgconn.PgError
	stalled := ctx.Err() != nil || pgconn.Timeout(err) || errors.Is(err, context.DeadlineExceeded)
	if errors.As(err, &pg) || !stalled {
		return "connect-error:" + errClass(err)
	}
	switch {
	case c.lookupStart.Load() >= 0 && c.lookupEnd.Load() < 0 && c.dialStart.Load() < 0:
		return "stalled-at-lookup"
	case c.dialStart.Load() < 0 || c.dialEnd.Load() < 0:
		return "stalled-at-dial"
	case c.firstByte.Load() < 0:
		return "stalled-awaiting-server"
	default:
		return "stalled-in-startup"
	}
}

// waitCount is one wait-event histogram bucket.
type waitCount struct {
	backendType string
	event       string
	n           int64
}

// serverSnapshot is a read-only view of the server's connection and
// checkpoint state. It never selects query, usename, client_addr,
// client_hostname or application_name.
type serverSnapshot struct {
	err               string // errClass of the first failed statement
	maxConnections    int64
	backends          int64
	clientBackends    int64
	perTestBackends   int64
	fhDatabases       int64
	templateDatabases int64
	uptimeSeconds     int64
	checkpointsReq    string // a number, or "n/a"
	checkpointsTimed  string
	checkpointerWait  string
	waits             []waitCount
}

const snapshotCountsSQL = `SELECT current_setting('max_connections')::bigint,
  (SELECT count(*) FROM pg_stat_activity),
  (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'),
  (SELECT count(*) FROM pg_stat_activity WHERE datname = $1),
  (SELECT count(*) FROM pg_database WHERE datname LIKE 'fh\_%'),
  (SELECT count(*) FROM pg_database WHERE datname LIKE 'fishhawk\_tmpl\_%'),
  floor(extract(epoch FROM now() - pg_postmaster_start_time()))::bigint`

const snapshotWaitsSQL = `SELECT coalesce(backend_type, '-'),
  coalesce(wait_event_type, '-') || ':' || coalesce(wait_event, '-'), count(*)
FROM pg_stat_activity GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2 LIMIT 5`

const snapshotCheckpointerSQL = `SELECT coalesce(wait_event_type, '-') || ':' || coalesce(wait_event, '-')
FROM pg_stat_activity WHERE backend_type = 'checkpointer'`

// snapshotCheckpointsSQL is a separate statement because PG17 moved these
// counters to pg_stat_checkpointer; an error renders n/a.
const snapshotCheckpointsSQL = `SELECT checkpoints_req, checkpoints_timed FROM pg_stat_bgwriter`

// takeSnapshot reads the server snapshot over conn. perTestDB is passed only
// as a bind parameter and is never rendered.
func takeSnapshot(ctx context.Context, conn *pgconn.PgConn, perTestDB string) serverSnapshot {
	snap := serverSnapshot{checkpointsReq: "n/a", checkpointsTimed: "n/a", checkpointerWait: "-"}
	rows, err := snapshotRows(ctx, conn, snapshotCountsSQL, perTestDB)
	if err != nil || len(rows) != 1 || len(rows[0]) != 7 {
		snap.err = errClass(err)
		return snap
	}
	dst := []*int64{&snap.maxConnections, &snap.backends, &snap.clientBackends, &snap.perTestBackends,
		&snap.fhDatabases, &snap.templateDatabases, &snap.uptimeSeconds}
	for i, p := range dst {
		*p, _ = strconv.ParseInt(string(rows[0][i]), 10, 64)
	}
	if rows, err := snapshotRows(ctx, conn, snapshotWaitsSQL); err == nil {
		for _, r := range rows {
			if len(r) != 3 {
				continue
			}
			n, _ := strconv.ParseInt(string(r[2]), 10, 64)
			snap.waits = append(snap.waits, waitCount{backendType: string(r[0]), event: string(r[1]), n: n})
		}
	}
	if rows, err := snapshotRows(ctx, conn, snapshotCheckpointerSQL); err == nil && len(rows) == 1 && len(rows[0]) == 1 {
		snap.checkpointerWait = string(rows[0][0])
	}
	if rows, err := snapshotRows(ctx, conn, snapshotCheckpointsSQL); err == nil && len(rows) == 1 && len(rows[0]) == 2 {
		snap.checkpointsReq = string(rows[0][0])
		snap.checkpointsTimed = string(rows[0][1])
	}
	return snap
}

func snapshotRows(ctx context.Context, conn *pgconn.PgConn, sql string, params ...string) ([][][]byte, error) {
	vals := make([][]byte, len(params))
	for i, p := range params {
		vals[i] = []byte(p)
	}
	res := conn.ExecParams(ctx, sql, vals, nil, nil, nil).Read()
	return res.Rows, res.Err
}

// diagReport is one probe round: both targets plus the server snapshot taken
// on the base probe's connection (nil when the base probe never got ready).
type diagReport struct {
	when    string // "live" or "post-failure"
	perTest probeResult
	base    probeResult
	server  *serverSnapshot
}

// runConnectProbe probes the per-test and base targets concurrently under
// ctx, so a stall in one cannot starve the other, taking the server snapshot
// through the BASE probe's onReady callback. At most two connections are
// opened. Both goroutines are joined before it returns.
func runConnectProbe(ctx context.Context, baseURL, perTestURL, perTestDB, when string, h probeHooks) diagReport {
	rep := diagReport{when: when}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		rep.perTest = probeTarget(ctx, perTestURL, h, nil)
		rep.perTest.target = "per-test"
	}()
	go func() {
		defer wg.Done()
		rep.base = probeTarget(ctx, baseURL, h, func(ctx context.Context, conn *pgconn.PgConn) {
			snap := takeSnapshot(ctx, conn, perTestDB)
			rep.server = &snap
		})
		rep.base.target = "base"
	}()
	wg.Wait()
	return rep
}

// renderDiag renders ONE credential-free line, capped at maxDiagBytes. Its
// vocabulary avoids the runner's infrastructure-flake markers so the suffix
// cannot change how a failure is classified.
func renderDiag(shape string, clone cloneTiming, connectElapsed time.Duration, rep *diagReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pgtest diagnostics: shape=%s clone{base=%s create=%s retries=%d} connect=%s",
		shape, ms(clone.baseConnect), ms(clone.create), clone.contentionRetries, ms(connectElapsed))
	if rep == nil {
		b.WriteString(" probe[none]")
	} else {
		fmt.Fprintf(&b, " probe[%s]{per-test:%s base:%s}", sanitizeToken(rep.when), renderProbe(rep.perTest), renderProbe(rep.base))
		b.WriteString(" server" + renderServer(rep.server))
	}
	out := b.String()
	if len(out) > maxDiagBytes {
		out = out[:maxDiagBytes-len(truncMarker)] + truncMarker
	}
	return out
}

func renderProbe(r probeResult) string {
	fams := "-"
	if len(r.families) > 0 {
		fams = strings.Join(r.families, ",")
	}
	return fmt.Sprintf("{outcome=%s lookup=%s addrs=%d fam=%s dials=%d dial=%s first-byte=%s ready=%s ping=%s budget=%s}",
		sanitizeToken(r.outcome), ms(r.lookup), r.addrs, fams, r.dials, ms(r.lastDial),
		ms(r.firstByte), ms(r.ready), ms(r.ping), ms(r.readyLimit))
}

func renderServer(s *serverSnapshot) string {
	if s == nil {
		return "{n/a}"
	}
	if s.err != "" {
		return "{err=" + sanitizeToken(s.err) + "}"
	}
	waits := make([]string, 0, maxWaitEntries)
	for i, w := range s.waits {
		if i == maxWaitEntries {
			break
		}
		waits = append(waits, fmt.Sprintf("%s/%s=%d", sanitizeToken(w.backendType), sanitizeToken(w.event), w.n))
	}
	return fmt.Sprintf("{max=%d backends=%d client=%d per-test=%d fh=%d tmpl=%d uptime=%ds ckpt-req=%s ckpt-timed=%s ckptr=%s waits[%s]}",
		s.maxConnections, s.backends, s.clientBackends, s.perTestBackends, s.fhDatabases, s.templateDatabases,
		s.uptimeSeconds, sanitizeToken(s.checkpointsReq), sanitizeToken(s.checkpointsTimed),
		sanitizeToken(s.checkpointerWait), strings.Join(waits, "|"))
}

// ms renders a duration in whole milliseconds; a negative duration (a phase
// never reached) renders "-".
func ms(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

// sanitizeToken keeps server-provided vocabulary (wait events, backend types,
// SQLSTATEs) to a short run of safe characters.
func sanitizeToken(s string) string {
	const maxLen = 64
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= maxLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ':', r == '_', r == '-', r == '=', r == '.', r == '/':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
	}
	return b.String()
}
