package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/dispatcherheartbeat"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// ADR-094 Amendment A5, the server half (#5614): the host-dispatch marker
// sizes host:<host>:capacity from the host's GOVERNING capacity, resolved
// once per request: a valid host_capacity in the FRESH heartbeat row of the
// run's exact account and the marker's label, else fishhawkd's
// --default-host-capacity, else 3. A fresh host_implement_limit caps
// local-implement:<host>. Cross-boundary: each test drives the REAL
// heartbeat handler (a host-dispatcher/<label> identity carrying write:runs,
// optionally account-bound) into the REAL Postgres heartbeat table, then the
// REAL marker handler against the Postgres slot store, and reads the 200/409
// bodies and the stage_concurrency_slots rows back.

// hcdT0 is the fixture clock's origin; the server clock (nowFunc) moves, the
// database clock is untouched, so freshness is decided on hcdT0 alone.
var hcdT0 = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

// hcdFixture is an scPG fixture with a heartbeat repository wired and a
// movable server clock.
type hcdFixture struct {
	*scPG
	now  time.Time
	logs *bytes.Buffer
}

// newHCD builds the fixture with fishhawkd's default capacity defaultCap (0:
// unset) over the Postgres heartbeat repository, or over repo when non-nil.
func newHCD(t *testing.T, defaultCap int, repo dispatcherheartbeat.Repository) *hcdFixture {
	t.Helper()
	logs := &bytes.Buffer{}
	f := &hcdFixture{now: hcdT0, logs: logs}
	f.scPG = newSCPGWith(t, pgStore, func(cfg *Config, pool *pgxpool.Pool) {
		cfg.DefaultHostCapacity = defaultCap
		cfg.HostDispatcherHeartbeatRepo = repo
		if repo == nil {
			cfg.HostDispatcherHeartbeatRepo = dispatcherheartbeat.NewPostgresRepository(pool)
		}
		cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	f.srv.nowFunc = func() time.Time { return f.now }
	return f
}

func hcdIntp(v int) *int { return &v }

// heartbeat POSTs a valid heartbeat as host-dispatcher/<label> (account ""
// = untenanted) with mut applied, through the real ingest handler, and
// returns the response.
func (f *hcdFixture) heartbeat(label, account string, mut func(*hostdispatcher.Heartbeat)) *httptest.ResponseRecorder {
	f.t.Helper()
	hb := hostdispatcher.Heartbeat{
		IntervalSeconds: 30, StartedAt: f.now.Add(-time.Hour), LastTickAt: f.now,
		SpawnsDay: f.now.Format("2006-01-02"), DailyCap: 3,
	}
	if mut != nil {
		mut(&hb)
	}
	raw, err := json.Marshal(hb)
	if err != nil {
		f.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/host-dispatchers/heartbeat", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: hostdispatcher.SubjectPrefix + label, TokenID: "tok-hd-" + label,
		Scopes: []string{hostdispatcher.ScopeWriteRuns}, AccountID: account,
	}))
	w := httptest.NewRecorder()
	f.srv.handleHostDispatcherHeartbeat(w, req)
	if w.Code != http.StatusOK {
		f.t.Fatalf("heartbeat %s = %d, want 200:\n%s", label, w.Code, w.Body.String())
	}
	return w
}

// declare POSTs a heartbeat declaring host_capacity capacity (0: absent) and
// host_implement_limit impl (0: absent).
func (f *hcdFixture) declare(label, account string, capacity, impl int) {
	f.t.Helper()
	f.heartbeat(label, account, func(h *hostdispatcher.Heartbeat) {
		if capacity != 0 {
			h.HostCapacity = hcdIntp(capacity)
		}
		if impl != 0 {
			h.HostImplementLimit = hcdIntp(impl)
		}
	})
}

// account inserts a tenant account row and returns its id.
func (f *hcdFixture) account() string {
	f.t.Helper()
	id := uuid.New()
	f.exec(`INSERT INTO accounts (id, account_key) VALUES ($1, $2)`, id, "acct-"+id.String()[:8])
	return id.String()
}

// tenantedStage is f.stage with the run bound to account.
func (f *hcdFixture) tenantedStage(account string, typ run.StageType) *run.Stage {
	f.t.Helper()
	st := f.stage(fhRepo, typ)
	f.exec(`UPDATE runs SET account_id = $1 WHERE id = $2`, account, st.RunID)
	return st
}

// admitPlans admits n plan stages (from mk) with body and asserts each holds
// host:<host>:capacity at limit and carries want as its capacity_warning.
func (f *hcdFixture) admitPlans(n int, mk func() *run.Stage, body, capKey string, limit int, want *hostCapacityWarning) {
	f.t.Helper()
	for i := range n {
		resp := f.admitted(mk(), body)
		if !hasGroupRef(resp.Concurrency.Groups, capKey, limit) {
			f.t.Fatalf("plan %d groups = %+v, want %s at limit %d", i+1, resp.Concurrency.Groups, capKey, limit)
		}
		if !sameWarning(resp.CapacityWarning, want) {
			f.t.Fatalf("plan %d capacity_warning = %+v, want %+v", i+1, resp.CapacityWarning, want)
		}
	}
}

// queuedOnCapacity asserts the stage queues on capKey at limit behind
// holders, with want as details.capacity_warning.
func (f *hcdFixture) queuedOnCapacity(st *run.Stage, body, capKey string, limit, holders int, want *hostCapacityWarning) {
	f.t.Helper()
	d, _, raw := f.queued(st, body)
	if d.Group != capKey || d.Limit != limit || len(d.Holders) != holders {
		f.t.Fatalf("queued details = %s, want %s at limit %d behind %d holders", raw, capKey, limit, holders)
	}
	if got := queuedCapacityWarning(f.t, raw); !sameWarning(got, want) {
		f.t.Fatalf("details.capacity_warning = %+v, want %+v:\n%s", got, want, raw)
	}
}

func sameWarning(got, want *hostCapacityWarning) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return *got == *want
}

func (f *hcdFixture) plan() *run.Stage { return f.stage(fhRepo, run.StageTypePlan) }

// TestHostCapacityDeclaration_DeclarationGovernsMarker (DONE-MEANS; CF1, CF7
// vehicle): a declared host_capacity 4 on h1 governs markers that send
// capacity 3. Four plans are admitted at limit 4, each 200 carrying
// capacity_warning {3, 4, declaration}; the fifth is 409 queued on
// host:h1:capacity at limit 4 with details.capacity_warning, and exactly four
// held rows exist. Honouring the request value queues the fourth at 3.
func TestHostCapacityDeclaration_DeclarationGovernsMarker(t *testing.T) {
	f := newHCD(t, 0, nil)
	f.declare("h1", "", 4, 0)
	want := &hostCapacityWarning{RequestCapacity: 3, GoverningCapacity: 4, Source: hostCapacitySourceDeclaration}
	body := capacityBody("h1", 3)
	f.admitPlans(4, f.plan, body, "host:h1:capacity", 4, want)
	f.queuedOnCapacity(f.plan(), body, "host:h1:capacity", 4, 4, want)
	if n := f.heldRows("host:h1:capacity"); n != 4 {
		t.Errorf("held host:h1:capacity rows = %d, want 4", n)
	}
}

// TestHostCapacityDeclaration_StaleDeclarationUsesFishhawkdDefault (CF2
// window arm, CF4): a declaration of 4 received at T0 with a 30s interval is
// stale at T0+2m (StaleAfter = 90s), so the fishhawkd default 2 governs: two
// plans are admitted and the third queues at 2, the warning naming
// fishhawkd_default. Reading the stale row as fresh admits the third.
func TestHostCapacityDeclaration_StaleDeclarationUsesFishhawkdDefault(t *testing.T) {
	f := newHCD(t, 2, nil)
	f.declare("h1", "", 4, 0)
	f.now = hcdT0.Add(2 * time.Minute)
	want := &hostCapacityWarning{RequestCapacity: 4, GoverningCapacity: 2, Source: hostCapacitySourceFishhawkdDefault}
	body := capacityBody("h1", 4)
	f.admitPlans(2, f.plan, body, "host:h1:capacity", 2, want)
	f.queuedOnCapacity(f.plan(), body, "host:h1:capacity", 2, 2, want)
}

// TestHostCapacityDeclaration_FutureDatedDeclarationUsesFishhawkdDefault
// (CF2 FutureSkew arm): a declaration received at T0+5m read at T0 is
// inside the staleness window (negative age) but beyond FutureSkew (60s), so
// only the future-skew arm rejects it: the fishhawkd default 2 governs.
func TestHostCapacityDeclaration_FutureDatedDeclarationUsesFishhawkdDefault(t *testing.T) {
	f := newHCD(t, 2, nil)
	f.now = hcdT0.Add(5 * time.Minute)
	f.declare("h1", "", 4, 0)
	f.now = hcdT0
	f.admitPlans(2, f.plan, hostBody("h1"), "host:h1:capacity", 2, nil)
	f.queuedOnCapacity(f.plan(), hostBody("h1"), "host:h1:capacity", 2, 2, nil)
}

// TestHostCapacityDeclaration_NeitherUsesDefaultThree: no declaration and no
// fishhawkd default: a body capacity 5 is admitted at 3 three times and the
// fourth queues at 3, each warning naming source default.
func TestHostCapacityDeclaration_NeitherUsesDefaultThree(t *testing.T) {
	f := newHCD(t, 0, nil)
	want := &hostCapacityWarning{RequestCapacity: 5, GoverningCapacity: concurrency.DefaultHostCapacity, Source: hostCapacitySourceDefault}
	body := capacityBody("h1", 5)
	f.admitPlans(concurrency.DefaultHostCapacity, f.plan, body, "host:h1:capacity", concurrency.DefaultHostCapacity, want)
	f.queuedOnCapacity(f.plan(), body, "host:h1:capacity", concurrency.DefaultHostCapacity, concurrency.DefaultHostCapacity, want)
}

// TestHostCapacityDeclaration_TenantedRunIgnoresUntenantedDeclaration (CF3,
// A5 item 7): an UNTENANTED declaration of 4 never governs a run bound to
// account A, so the fishhawkd default 2 does: the third plan queues at 2.
// List's visibility rule (untenanted rows visible to every account) would
// admit it at 4.
func TestHostCapacityDeclaration_TenantedRunIgnoresUntenantedDeclaration(t *testing.T) {
	f := newHCD(t, 2, nil)
	acct := f.account()
	f.declare("h1", "", 4, 0)
	mk := func() *run.Stage { return f.tenantedStage(acct, run.StageTypePlan) }
	f.admitPlans(2, mk, hostBody("h1"), "host:h1:capacity", 2, nil)
	f.queuedOnCapacity(mk(), hostBody("h1"), "host:h1:capacity", 2, 2, nil)
}

// TestHostCapacityDeclaration_SameAccountDeclarationGoverns: an account-A
// dispatcher's declaration of 4 governs an account-A run: four plans are
// admitted and the fifth queues at 4.
func TestHostCapacityDeclaration_SameAccountDeclarationGoverns(t *testing.T) {
	f := newHCD(t, 2, nil)
	acct := f.account()
	f.declare("h1", acct, 4, 0)
	mk := func() *run.Stage { return f.tenantedStage(acct, run.StageTypePlan) }
	f.admitPlans(4, mk, hostBody("h1"), "host:h1:capacity", 4, nil)
	f.queuedOnCapacity(mk(), hostBody("h1"), "host:h1:capacity", 4, 4, nil)
}

// TestHostCapacityDeclaration_ImplementOnlyDeclarationLeavesCapacityOnFishhawkdDefault
// (CF5): {host_implement_limit: 1} alone leaves capacity on the fishhawkd
// default 2: the third plan queues on host:h1:capacity at 2, not at 3.
func TestHostCapacityDeclaration_ImplementOnlyDeclarationLeavesCapacityOnFishhawkdDefault(t *testing.T) {
	f := newHCD(t, 2, nil)
	f.declare("h1", "", 0, 1)
	want := &hostCapacityWarning{RequestCapacity: 3, GoverningCapacity: 2, Source: hostCapacitySourceFishhawkdDefault}
	body := capacityBody("h1", 3)
	f.admitPlans(2, f.plan, body, "host:h1:capacity", 2, want)
	f.queuedOnCapacity(f.plan(), body, "host:h1:capacity", 2, 2, want)
}

// TestHostCapacityDeclaration_ImplementLimitIsCeiling (CF6): a fresh
// host_implement_limit caps local-implement:<host> at min(spec, declared).
// Spec {limit: 2} with a declared 1: the second implement queues on
// local-implement:h1 at limit 1. Spec default 1 with a declared 4: the
// second implement still queues at 1 — the ceiling never raises.
func TestHostCapacityDeclaration_ImplementLimitIsCeiling(t *testing.T) {
	for _, tc := range []struct {
		name     string
		block    string
		declared int
	}{
		{"declared below spec", "\n          limit: 2", 1},
		{"declared above spec default", "", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHCD(t, 0, nil)
			f.declare("h1", "", 0, tc.declared)
			doc := mustParseSpec(t, typedSpec(run.StageTypeImplement, tc.block))
			resp := f.admitted(f.specStageOf(fhRepo, run.StageTypeImplement, doc), hostBody("h1"))
			if !hasGroupRef(resp.Concurrency.Groups, "local-implement:h1", 1) {
				t.Fatalf("first implement groups = %+v, want local-implement:h1 at limit 1", resp.Concurrency.Groups)
			}
			d, _, raw := f.queued(f.specStageOf(fhRepo, run.StageTypeImplement, doc), hostBody("h1"))
			if d.Group != "local-implement:h1" || d.Limit != 1 || len(d.Holders) != 1 {
				t.Fatalf("second implement details = %s, want queued on local-implement:h1 at limit 1 behind 1 holder", raw)
			}
		})
	}
}

// TestHostCapacityDeclaration_OmittedDeclarationReverts (C7): a heartbeat
// that omits host_capacity removes the earlier declaration of 4, so the
// fishhawkd default 2 governs the next marker.
func TestHostCapacityDeclaration_OmittedDeclarationReverts(t *testing.T) {
	f := newHCD(t, 2, nil)
	f.declare("h1", "", 4, 0)
	f.declare("h1", "", 0, 0)
	f.admitPlans(2, f.plan, hostBody("h1"), "host:h1:capacity", 2, nil)
	f.queuedOnCapacity(f.plan(), hostBody("h1"), "host:h1:capacity", 2, 2, nil)
}

// TestHostCapacityDeclaration_OutOfRangeNotCarriedForward (C7): after a
// declaration of 4, a heartbeat declaring 65 drops the whole declaration
// (dropped_declaration host_capacity, recorded on the row) and carries
// nothing forward: the fishhawkd default 2 governs.
func TestHostCapacityDeclaration_OutOfRangeNotCarriedForward(t *testing.T) {
	f := newHCD(t, 2, nil)
	f.declare("h1", "", 4, 0)
	w := f.heartbeat("h1", "", func(h *hostdispatcher.Heartbeat) { h.HostCapacity = hcdIntp(65) })
	var resp hostdispatcher.HeartbeatResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.DroppedDeclaration != hostdispatcher.DeclarationFieldHostCapacity {
		t.Fatalf("heartbeat response = %s (%v), want dropped_declaration host_capacity", w.Body.String(), err)
	}
	rec, found, err := dispatcherheartbeat.NewPostgresRepository(f.pool).Get(context.Background(), "", "h1")
	if err != nil || !found || rec.Heartbeat.HostCapacity != nil || rec.DroppedDeclaration != hostdispatcher.DeclarationFieldHostCapacity {
		t.Fatalf("stored row = %+v found=%v err=%v, want host_capacity nil and dropped_declaration host_capacity", rec, found, err)
	}
	f.admitPlans(2, f.plan, hostBody("h1"), "host:h1:capacity", 2, nil)
	f.queuedOnCapacity(f.plan(), hostBody("h1"), "host:h1:capacity", 2, 2, nil)
}

// TestHostCapacityDeclaration_ReadErrorFallsBack (CF15, C5): a heartbeat
// read error WARN-logs and falls through to the fishhawkd default 2: two
// plans are admitted and the third queues at 2. The fall-through ALSO drops
// the row's fresh host_implement_limit 1, so on h2 two implements of a spec
// {limit: 2} are both admitted at local-implement:h2 limit 2 (the spec
// value). The row is seeded by construction, fresh at T0; the zero
// hostLimits (capacity 0) would fail Admit.
func TestHostCapacityDeclaration_ReadErrorFallsBack(t *testing.T) {
	mem := dispatcherheartbeat.NewMemoryRepository()
	for _, label := range []string{"h1", "h2"} {
		if err := mem.Upsert(context.Background(), dispatcherheartbeat.Record{
			HostLabel: label, ReceivedAt: hcdT0,
			Heartbeat: hostdispatcher.Heartbeat{IntervalSeconds: 30, HostCapacity: hcdIntp(4), HostImplementLimit: hcdIntp(1)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	mem.GetErr = errors.New("heartbeat store unavailable")
	f := newHCD(t, 2, mem)
	f.admitPlans(2, f.plan, hostBody("h1"), "host:h1:capacity", 2, nil)
	f.queuedOnCapacity(f.plan(), hostBody("h1"), "host:h1:capacity", 2, 2, nil)
	if !strings.Contains(f.logs.String(), "heartbeat declaration read failed") || !strings.Contains(f.logs.String(), "level=WARN") {
		t.Errorf("logs = %q, want a WARN naming the heartbeat declaration read failure", f.logs.String())
	}

	doc := mustParseSpec(t, typedSpec(run.StageTypeImplement, "\n          limit: 2"))
	for i := range 2 {
		resp := f.admitted(f.specStageOf(fhRepo, run.StageTypeImplement, doc), hostBody("h2"))
		if !hasGroupRef(resp.Concurrency.Groups, "local-implement:h2", 2) {
			t.Fatalf("implement %d groups = %+v, want local-implement:h2 at the spec limit 2 (no declared ceiling while the read fails)", i+1, resp.Concurrency.Groups)
		}
	}
}

// TestHostCapacityDeclaration_NoWarningWhenEqualOrAbsent: no
// capacity_warning when the request's capacity equals the governing one, or
// when the request sends none.
func TestHostCapacityDeclaration_NoWarningWhenEqualOrAbsent(t *testing.T) {
	f := newHCD(t, 0, nil)
	f.declare("h1", "", 4, 0)
	f.admitPlans(1, f.plan, capacityBody("h1", 4), "host:h1:capacity", 4, nil)
	f.admitPlans(1, f.plan, hostBody("h1"), "host:h1:capacity", 4, nil)
	f.admitPlans(1, f.plan, capacityBody("h2", concurrency.DefaultHostCapacity), "host:h2:capacity", concurrency.DefaultHostCapacity, nil)
}

// TestHostCapacityDeclaration_EarlierHoldersKeepHolding: three plans are
// admitted under a declaration of 4; a declaration of 2 then lowers the
// governing value. All three held rows stay held, and a new plan queues on
// host:h1:capacity at limit 2 behind the three holders.
func TestHostCapacityDeclaration_EarlierHoldersKeepHolding(t *testing.T) {
	f := newHCD(t, 0, nil)
	f.declare("h1", "", 4, 0)
	held := []*run.Stage{f.plan(), f.plan(), f.plan()}
	for _, st := range held {
		f.admitted(st, hostBody("h1"))
	}
	f.declare("h1", "", 2, 0)
	f.queuedOnCapacity(f.plan(), hostBody("h1"), "host:h1:capacity", 2, 3, nil)
	for _, st := range held {
		if rows := f.slotRows(st.ID); len(rows) != 1 || rows[0].state != "held" {
			t.Errorf("holder %s slot rows = %+v, want one held row", st.ID, rows)
		}
	}
	if n := f.heldRows("host:h1:capacity"); n != 3 {
		t.Errorf("held host:h1:capacity rows = %d, want 3", n)
	}
}

// TestResolveHostLimits_Guards pins the ladder's defensive arms on rows
// seeded BY CONSTRUCTION (bypassing ingest's drop rule): a stored
// host_capacity or host_implement_limit outside 1..64 never sizes a group, a
// fishhawkd default outside 1..64 is read as unset, a stale row is skipped,
// and a nil heartbeat repository skips rung (a).
func TestResolveHostLimits_Guards(t *testing.T) {
	runRow := &run.Run{ID: uuid.New()}
	for _, tc := range []struct {
		name       string
		nilRepo    bool
		rec        *hostdispatcher.Heartbeat
		receivedAt time.Time
		defaultCap int
		want       hostLimits
	}{
		{name: "nil repo, no default", nilRepo: true, want: hostLimits{capacity: 3, source: hostCapacitySourceDefault}},
		{name: "nil repo, fishhawkd default", nilRepo: true, defaultCap: 5, want: hostLimits{capacity: 5, source: hostCapacitySourceFishhawkdDefault}},
		{name: "fishhawkd default above max", defaultCap: 65, want: hostLimits{capacity: 3, source: hostCapacitySourceDefault}},
		{name: "fishhawkd default negative", defaultCap: -1, want: hostLimits{capacity: 3, source: hostCapacitySourceDefault}},
		{name: "fishhawkd default max", defaultCap: concurrency.MaxLimit, want: hostLimits{capacity: concurrency.MaxLimit, source: hostCapacitySourceFishhawkdDefault}},
		{name: "fresh valid declaration", rec: &hostdispatcher.Heartbeat{IntervalSeconds: 30, HostCapacity: hcdIntp(7), HostImplementLimit: hcdIntp(2)}, receivedAt: hcdT0, defaultCap: 2,
			want: hostLimits{capacity: 7, source: hostCapacitySourceDeclaration, implementLimit: 2}},
		{name: "stored capacity above max", rec: &hostdispatcher.Heartbeat{IntervalSeconds: 30, HostCapacity: hcdIntp(65)}, receivedAt: hcdT0, defaultCap: 2,
			want: hostLimits{capacity: 2, source: hostCapacitySourceFishhawkdDefault}},
		{name: "stored capacity zero", rec: &hostdispatcher.Heartbeat{IntervalSeconds: 30, HostCapacity: hcdIntp(0)}, receivedAt: hcdT0, defaultCap: 2,
			want: hostLimits{capacity: 2, source: hostCapacitySourceFishhawkdDefault}},
		{name: "stored implement limit above max", rec: &hostdispatcher.Heartbeat{IntervalSeconds: 30, HostImplementLimit: hcdIntp(65)}, receivedAt: hcdT0, defaultCap: 2,
			want: hostLimits{capacity: 2, source: hostCapacitySourceFishhawkdDefault}},
		{name: "stale row", rec: &hostdispatcher.Heartbeat{IntervalSeconds: 30, HostCapacity: hcdIntp(7), HostImplementLimit: hcdIntp(2)}, receivedAt: hcdT0.Add(-2 * time.Minute), defaultCap: 2,
			want: hostLimits{capacity: 2, source: hostCapacitySourceFishhawkdDefault}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Addr: "127.0.0.1:0", DefaultHostCapacity: tc.defaultCap}
			if !tc.nilRepo {
				mem := dispatcherheartbeat.NewMemoryRepository()
				if tc.rec != nil {
					if err := mem.Upsert(context.Background(), dispatcherheartbeat.Record{HostLabel: "h1", Heartbeat: *tc.rec, ReceivedAt: tc.receivedAt}); err != nil {
						t.Fatal(err)
					}
				}
				cfg.HostDispatcherHeartbeatRepo = mem
			}
			s := New(cfg)
			s.nowFunc = func() time.Time { return hcdT0 }
			if got := s.resolveHostLimits(context.Background(), runRow, "h1"); got != tc.want {
				t.Fatalf("resolveHostLimits = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestApplyHostImplementCeiling: no ceiling (0) leaves every limit alone; a
// ceiling lowers only local-implement:<host>, never raises it, and never
// touches another host's group or the capacity group.
func TestApplyHostImplementCeiling(t *testing.T) {
	mk := func() []concurrency.GroupRequest {
		return []concurrency.GroupRequest{
			{Key: "local-implement:h1", Limit: 2}, {Key: "local-implement:h2", Limit: 2}, {Key: "host:h1:capacity", Limit: 5},
		}
	}
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{{"none", 0, 2}, {"lowers", 1, 1}, {"never raises", 4, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			got := applyHostImplementCeiling(mk(), "h1", tc.limit)
			want := mk()
			want[0].Limit = tc.want
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("applyHostImplementCeiling(%d) = %+v, want %+v", tc.limit, got, want)
				}
			}
		})
	}
}
