package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/dispatcherheartbeat"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// host_dispatcher_heartbeat_e2e_test.go is the #5489 cross-boundary e2e: the
// REAL hostDispatchLoop and its heartbeat publisher, under the harness's real
// host-dispatcher/e2e-host token, POST over real HTTP to the real server,
// which upserts into the real Postgres host_dispatcher_heartbeats table; the
// campaign status awaiting block is then read through the real MCP client
// (including its host_dispatcher mirror) and a raw operator GET.

const adE2EHeartbeatHost = "e2e-host"

// newAdE2EHeartbeatHarness is the plan-spec harness with the Postgres
// heartbeat repository wired and the loop's real publisher started; the
// returned channel receives every publish attempt.
func newAdE2EHeartbeatHarness(t *testing.T) (*adE2EHarness, <-chan hdHeartbeatDelivery) {
	t.Helper()
	h := newAdE2EHarnessWith(t, adE2EPlanSpecYAML, func(cfg *server.Config, pool *pgxpool.Pool) {
		cfg.HostDispatcherHeartbeatRepo = dispatcherheartbeat.NewPostgresRepository(pool)
	})
	got, _, _ := hdStartPublisher(t, h.loop)
	return h, got
}

// heartbeatTick runs one dispatcher tick and waits for its heartbeat POST,
// which must succeed.
func (h *adE2EHarness) heartbeatTick(got <-chan hdHeartbeatDelivery) {
	h.t.Helper()
	h.dispatcherTick()
	if d := hdAwaitDelivery(h.t, got, "dispatcher tick"); d.err != nil {
		h.t.Fatalf("heartbeat POST failed: %v", d.err)
	}
}

// rawAwaiting reads GET /v0/campaigns/{id}/status under the operator bearer
// and returns the item's awaiting block as decoded JSON.
func (h *adE2EHarness) rawAwaiting() map[string]any {
	h.t.Helper()
	req, err := http.NewRequestWithContext(h.ctx, http.MethodGet, h.ts.URL+"/v0/campaigns/"+h.campaign.ID.String()+"/status", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.operatorBearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("raw campaign status: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("raw campaign status = %d", resp.StatusCode)
	}
	var body struct {
		Items []struct {
			IssueRef string         `json:"issue_ref"`
			Awaiting map[string]any `json:"awaiting"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		h.t.Fatalf("decode raw campaign status: %v", err)
	}
	for _, it := range body.Items {
		if it.IssueRef == h.item.IssueRef {
			return it.Awaiting
		}
	}
	h.t.Fatalf("raw campaign status has no item %s", h.item.IssueRef)
	return nil
}

// wantHeartbeatBlock asserts the MCP client's block reads kind/reason with
// host_dispatcher.state, and that the raw operator GET agrees.
func (h *adE2EHarness) wantHeartbeatBlock(step, kind, reason, state string) *campaignItemAwaiting {
	h.t.Helper()
	_, blk := h.status(h.operatorBearer)
	if blk == nil || blk.Kind != kind || blk.Reason != reason {
		h.t.Fatalf("%s: MCP awaiting = %+v; want %s/%s", step, blk, kind, reason)
	}
	if blk.HostDispatcher == nil || blk.HostDispatcher.State != state {
		h.t.Fatalf("%s: MCP host_dispatcher = %+v; want state %s", step, blk.HostDispatcher, state)
	}
	raw := h.rawAwaiting()
	hd, _ := raw["host_dispatcher"].(map[string]any)
	if raw["kind"] != kind || raw["reason"] != reason || hd == nil || hd["state"] != state {
		h.t.Fatalf("%s: raw awaiting = %v; want %s/%s with host_dispatcher.state %s", step, raw, kind, reason, state)
	}
	return blk
}

// TestHostDispatcherHeartbeatE2E_AwaitingNamesDispatcherState is #5489 test
// 32. (a) Before any dispatcher tick the parked plan item reads
// host_dispatcher_absent. (b) A paused tick's heartbeat reads
// host_dispatch_paused naming the harness host. (c) Resumed, the plan spawns
// and the item leaves the host_dispatch reasons. (d) With a daily cap of 1
// already spent, the parked implement stage reads host_dispatch_capped with
// cap_resets_at. (e) A working_dir outside the allowed roots reads
// host_dispatch_refused, refusing path_outside_allowed_roots. (f) A heartbeat
// aged 10 minutes reads host_dispatcher_absent, state stale.
func TestHostDispatcherHeartbeatE2E_AwaitingNamesDispatcherState(t *testing.T) {
	h, got := newAdE2EHeartbeatHarness(t)
	h.loop.cfg.DailyCap = 1
	h.loop.status.DailyCap = 1

	// (a) no heartbeat yet.
	h.driverTick()
	runID := h.runID()
	plan := h.stageOf(runID, runpkg.StageTypePlan)
	if blk := h.wantHeartbeatBlock("(a) before any tick", "human", "host_dispatcher_absent", "absent"); blk.StageID != plan.ID.String() {
		t.Fatalf("(a) awaiting stage = %s; want the plan stage %s", blk.StageID, plan.ID)
	}

	// (b) paused.
	if err := SetHostDispatcherPaused(h.loop.cfg.StateDir, true); err != nil {
		t.Fatal(err)
	}
	h.heartbeatTick(got)
	if blk := h.wantHeartbeatBlock("(b) paused", "human", "host_dispatch_paused", "paused"); blk.HostDispatcher.Host != adE2EHeartbeatHost {
		t.Fatalf("(b) host_dispatcher.host = %q; want %q", blk.HostDispatcher.Host, adE2EHeartbeatHost)
	}
	if n := len(h.spawner.recorded()); n != 0 {
		t.Fatalf("(b) spawns = %d while paused; want 0", n)
	}

	// (c) resumed: the plan spawns and parks at its gate.
	if err := SetHostDispatcherPaused(h.loop.cfg.StateDir, false); err != nil {
		t.Fatal(err)
	}
	h.heartbeatTick(got)
	if st := h.stageOf(runID, runpkg.StageTypePlan); st.State != runpkg.StageStateAwaitingApproval {
		t.Fatalf("(c) plan stage = %s after the resumed tick; want awaiting_approval", st.State)
	}
	if _, blk := h.status(h.operatorBearer); blk == nil || strings.HasPrefix(blk.Reason, "host_dispatch") || blk.Kind == "host_dispatcher" {
		t.Fatalf("(c) awaiting = %+v; want the item off the host_dispatch reasons", blk)
	}

	// (d) the cap of 1 is spent by the plan spawn: the approved plan's
	// implement stage reads capped.
	if code, ec := h.postApproval(h.operatorBearer, plan.ID); code >= 300 {
		t.Fatalf("(d) human plan approval = %d %s", code, ec)
	}
	h.heartbeatTick(got)
	capped := h.wantHeartbeatBlock("(d) capped", "human", "host_dispatch_capped", "capped")
	if capped.HostDispatcher.CapResetsAt.IsZero() || capped.HostDispatcher.DailyCap != 1 {
		t.Fatalf("(d) host_dispatcher = %+v; want daily_cap 1 and cap_resets_at", capped.HostDispatcher)
	}
	if got := spawnTypes(h.spawner.recorded()); strings.Join(got, ",") != "plan" {
		t.Fatalf("(d) spawns = %v; want only the plan under the cap", got)
	}

	// (e) the run's working_dir is outside the dispatcher's allowed roots.
	h.loop.core.r.allowedRoots = []string{filepath.Join(h.root, "elsewhere")}
	h.heartbeatTick(got)
	refused := h.wantHeartbeatBlock("(e) refused", "human", "host_dispatch_refused", "refusing")
	if refused.HostDispatcher.RefusalCode != hdRefusePathOutside {
		t.Fatalf("(e) refusal_code = %q; want %s", refused.HostDispatcher.RefusalCode, hdRefusePathOutside)
	}

	// (f) the heartbeat ages past three intervals.
	if _, err := h.pool.Exec(context.Background(), `UPDATE host_dispatcher_heartbeats SET received_at = $1`, time.Now().Add(-10*time.Minute)); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}
	stale := h.wantHeartbeatBlock("(f) stale", "human", "host_dispatcher_absent", "stale")
	if stale.HostDispatcher.Host != adE2EHeartbeatHost || stale.HostDispatcher.LastHeartbeatAt.IsZero() {
		t.Fatalf("(f) host_dispatcher = %+v; want host %s and last_heartbeat_at", stale.HostDispatcher, adE2EHeartbeatHost)
	}
	if !strings.HasSuffix(adE2EDispatcherSub, "/"+adE2EHeartbeatHost) || !hostdispatcher.RouteAllowed(http.MethodPost, hostdispatcher.HeartbeatRoute) {
		t.Fatal("harness dispatcher subject or allowlist drifted")
	}
}

// TestHostDispatcherHeartbeatE2E_EchoesHostLabelAndStoresDeclaration (#5614,
// ADR-094 A5) is cross-boundary: the REAL publish seam (postHeartbeat) under
// the harness's real host-dispatcher/e2e-host token POSTs over real HTTP to
// the real server and Postgres. (a) The 200 echoes host_label e2e-host and an
// in-range declaration lands in the row. (b) An out-of-range declaration
// answers dropped_declaration and the row keeps no declaration but records the
// dropped field. (c) Through the real publisher, a loop whose resolved label
// matches the token logs no label warning; (d) one whose resolved label
// differs logs exactly one, naming the server-echoed token label.
func TestHostDispatcherHeartbeatE2E_EchoesHostLabelAndStoresDeclaration(t *testing.T) {
	var repo *dispatcherheartbeat.PostgresRepository
	h := newAdE2EHarnessWith(t, adE2EPlanSpecYAML, func(cfg *server.Config, pool *pgxpool.Pool) {
		repo = dispatcherheartbeat.NewPostgresRepository(pool)
		cfg.HostDispatcherHeartbeatRepo = repo
	})
	now := time.Now().UTC()
	hb := func(capacity, implement int) hostdispatcher.Heartbeat {
		return hostdispatcher.Heartbeat{
			IntervalSeconds: 30, StartedAt: now.Add(-time.Minute), LastTickAt: now,
			HostCapacity: &capacity, HostImplementLimit: &implement,
		}
	}
	row := func(step string) dispatcherheartbeat.Record {
		t.Helper()
		rec, found, err := repo.Get(context.Background(), "", adE2EHeartbeatHost)
		if err != nil || !found {
			t.Fatalf("%s: Get(untenanted, %s) found=%v err=%v; want the dispatcher's row", step, adE2EHeartbeatHost, found, err)
		}
		return rec
	}

	// (a) in range.
	resp, err := h.loop.postHeartbeat(h.ctx, hb(4, 2))
	if err != nil {
		t.Fatalf("(a) postHeartbeat: %v", err)
	}
	if resp.HostLabel != adE2EHeartbeatHost || resp.DroppedDeclaration != "" {
		t.Fatalf("(a) response = %+v; want host_label %s and no dropped_declaration", resp, adE2EHeartbeatHost)
	}
	if rec := row("(a)"); rec.Heartbeat.HostCapacity == nil || *rec.Heartbeat.HostCapacity != 4 ||
		rec.Heartbeat.HostImplementLimit == nil || *rec.Heartbeat.HostImplementLimit != 2 || rec.DroppedDeclaration != "" {
		t.Fatalf("(a) stored = %+v dropped %q; want host_capacity 4, host_implement_limit 2", rec.Heartbeat, rec.DroppedDeclaration)
	}

	// (b) out of range: the whole declaration drops, the heartbeat stays.
	resp, err = h.loop.postHeartbeat(h.ctx, hb(4, 65))
	if err != nil {
		t.Fatalf("(b) postHeartbeat: %v", err)
	}
	if resp.DroppedDeclaration != hostdispatcher.DeclarationFieldHostImplementLimit || resp.HostLabel != adE2EHeartbeatHost {
		t.Fatalf("(b) response = %+v; want dropped_declaration host_implement_limit", resp)
	}
	if rec := row("(b)"); rec.Heartbeat.HostCapacity != nil || rec.Heartbeat.HostImplementLimit != nil ||
		rec.DroppedDeclaration != hostdispatcher.DeclarationFieldHostImplementLimit {
		t.Fatalf("(b) stored = %+v dropped %q; want no declaration carried forward and the dropped field recorded", rec.Heartbeat, rec.DroppedDeclaration)
	}

	// (c) and (d): the real publisher's label check against the real echo.
	const warn = "ADR-094 A5 matches a declaration's label exactly"
	log := &hdLockedLog{}
	h.loop.cfg.Log = log
	h.loop.hostLabel = adE2EHeartbeatHost
	got, _, _ := hdStartPublisher(t, h.loop)
	h.heartbeatTick(got)
	if n := log.count(warn); n != 0 {
		t.Fatalf("(c) matching labels logged %d warnings; want 0\n%s", n, log.b.String())
	}
	h.loop.hostLabel = "Mac"
	h.heartbeatTick(got)
	h.heartbeatTick(got)
	if n := log.count(warn); n != 1 || !strings.Contains(log.b.String(), "host-dispatcher/"+adE2EHeartbeatHost) {
		t.Fatalf("(d) mismatched labels logged %d warnings; want 1 naming host-dispatcher/%s\n%s", n, adE2EHeartbeatHost, log.b.String())
	}
}
