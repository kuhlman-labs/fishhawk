package dispatcherheartbeat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

const (
	acctX = "6f1c2d3e-4a5b-4c6d-8e7f-90a1b2c3d4e5"
	acctY = "7a2d3e4f-5b6c-4d7e-9f80-a1b2c3d4e5f6"
	runA  = "0b6a3b8e-0d2f-4a38-9a66-1f5bfb3c2d10"
)

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func hb(paused bool) hostdispatcher.Heartbeat {
	return hostdispatcher.Heartbeat{
		IntervalSeconds: 30, StartedAt: t0.Add(-time.Hour), LastTickAt: t0, Paused: paused,
		SpawnsToday: 2, SpawnsDay: "2026-11-24", DailyCap: 3, InFlight: 1,
		RunRefusals: []hostdispatcher.RunRefusal{{
			RunID: runA, Code: "budget_blocked", Disposition: hostdispatcher.DispositionBudgetBlocked,
			BudgetSource: hostdispatcher.BudgetSourceLiveSpec, FirstAt: t0, LastAt: t0,
		}},
	}
}

func labels(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.AccountID + "/" + r.HostLabel
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPostgresRepository_UpsertListOrder (pgtest): an upsert replaces the row
// for the same (account, label); List orders newest received_at first, ties
// by host_label; the limit is honoured; the payload round-trips.
func TestPostgresRepository_UpsertListOrder(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()

	for _, rec := range []Record{
		{HostLabel: "b-host", TokenID: "tok-1", Heartbeat: hb(false), ReceivedAt: t0},
		{HostLabel: "a-host", Heartbeat: hb(false), ReceivedAt: t0},
		{HostLabel: "c-host", Heartbeat: hb(false), ReceivedAt: t0.Add(-time.Minute)},
		// Replaces b-host's row: newer, paused, new token.
		{HostLabel: "b-host", TokenID: "tok-2", Heartbeat: hb(true), ReceivedAt: t0.Add(time.Minute)},
	} {
		if err := repo.Upsert(ctx, rec); err != nil {
			t.Fatalf("Upsert(%s): %v", rec.HostLabel, err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_dispatcher_heartbeats`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("row count = %d (%v), want 3 (the second b-host upsert replaced its row)", n, err)
	}
	got, err := repo.List(ctx, "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if want := []string{"/b-host", "/a-host", "/c-host"}; !equalStrings(labels(got), want) {
		t.Fatalf("List order = %v, want %v (received_at DESC, then host_label)", labels(got), want)
	}
	b := got[0]
	if !b.Heartbeat.Paused || b.TokenID != "tok-2" || !b.ReceivedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("b-host = paused %v token %q received %v; want the replacing upsert", b.Heartbeat.Paused, b.TokenID, b.ReceivedAt)
	}
	if len(b.Heartbeat.RunRefusals) != 1 || b.Heartbeat.RunRefusals[0].BudgetSource != hostdispatcher.BudgetSourceLiveSpec ||
		b.Heartbeat.DailyCap != 3 || b.Heartbeat.SpawnsDay != "2026-11-24" {
		t.Errorf("payload did not round-trip: %+v", b.Heartbeat)
	}
	if got[1].TokenID != "" {
		t.Errorf("a-host token_id = %q, want empty (stored NULL)", got[1].TokenID)
	}
	if lim, err := repo.List(ctx, "", 2); err != nil || len(lim) != 2 {
		t.Errorf("List limit 2 = %d rows (%v), want 2", len(lim), err)
	}
}

// TestPostgresRepository_AccountPartition (pgtest), seeded with (NULL, a),
// (X, a) and (Y, b). pgtest connects as the table owner and the runtime
// superuser bypasses RLS, so the SQL WHERE is what isolates. Counterfactual run
// by hand: dropping the WHERE predicate made Y's row (and X's row for the
// untenanted read) visible, RED.
func TestPostgresRepository_AccountPartition(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	for _, rec := range []Record{
		{HostLabel: "a", Heartbeat: hb(false), ReceivedAt: t0},
		{AccountID: acctX, HostLabel: "a", Heartbeat: hb(true), ReceivedAt: t0.Add(-time.Second)},
		{AccountID: acctY, HostLabel: "b", Heartbeat: hb(true), ReceivedAt: t0.Add(-2 * time.Second)},
	} {
		if err := repo.Upsert(ctx, rec); err != nil {
			t.Fatalf("Upsert(%s/%s): %v", rec.AccountID, rec.HostLabel, err)
		}
	}
	for _, c := range []struct {
		account string
		want    []string
	}{
		{"", []string{"/a"}},
		{acctX, []string{"/a", acctX + "/a"}},
		{acctY, []string{"/a", acctY + "/b"}},
	} {
		got, err := repo.List(ctx, c.account, 32)
		if err != nil {
			t.Fatalf("List(%q): %v", c.account, err)
		}
		if !equalStrings(labels(got), c.want) {
			t.Errorf("List(%q) = %v, want %v", c.account, labels(got), c.want)
		}
	}
	if _, err := repo.List(ctx, "not-a-uuid", 32); err == nil {
		t.Error("List(non-UUID account) = nil error, want a refusal before the database")
	}
}

// TestMemoryRepository_Semantics pins the test double to the Postgres
// repository's account semantics, ordering, limit and error injection.
func TestMemoryRepository_Semantics(t *testing.T) {
	m := NewMemoryRepository()
	ctx := context.Background()
	for _, rec := range []Record{
		{HostLabel: "a", Heartbeat: hb(false), ReceivedAt: t0},
		{AccountID: acctX, HostLabel: "a", Heartbeat: hb(true), ReceivedAt: t0.Add(-time.Second)},
		{AccountID: acctY, HostLabel: "b", Heartbeat: hb(true), ReceivedAt: t0.Add(-2 * time.Second)},
		{HostLabel: "a", Heartbeat: hb(true), ReceivedAt: t0.Add(time.Second)},
	} {
		if err := m.Upsert(ctx, rec); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	if n := len(m.Records()); n != 3 {
		t.Fatalf("Records() = %d, want 3 (upsert replaced untenanted a)", n)
	}
	for _, c := range []struct {
		account string
		want    []string
	}{
		{"", []string{"/a"}},
		{acctX, []string{"/a", acctX + "/a"}},
	} {
		got, err := m.List(ctx, c.account, 32)
		if err != nil || !equalStrings(labels(got), c.want) {
			t.Errorf("List(%q) = %v (%v), want %v", c.account, labels(got), err, c.want)
		}
	}
	if got, _ := m.List(ctx, acctX, 1); len(got) != 1 || !got[0].Heartbeat.Paused {
		t.Errorf("List limit 1 = %v, want the newest row only", labels(got))
	}
	boom := errors.New("boom")
	m.ListErr = boom
	if _, err := m.List(ctx, "", 32); !errors.Is(err, boom) {
		t.Errorf("List with ListErr = %v, want boom", err)
	}
	if m.ListCalls() != 4 {
		t.Errorf("ListCalls = %d, want 4 (erroring calls count)", m.ListCalls())
	}
	m.UpsertErr = boom
	if err := m.Upsert(ctx, Record{HostLabel: "z", ReceivedAt: t0}); !errors.Is(err, boom) {
		t.Errorf("Upsert with UpsertErr = %v, want boom", err)
	}
}

// getExactCases is the shared Get table: an untenanted and an acctX row share
// label "a" (seeded by the caller with host capacity 1 and 2 respectively).
var getExactCases = []struct {
	name, account, label string
	wantFound            bool
	wantCap              int
}{
	{"untenanted caller gets the untenanted row only", "", "a", true, 1},
	{"tenanted caller gets its own row only", acctX, "a", true, 2},
	{"other account finds nothing (no untenanted fallback)", acctY, "a", false, 0},
	{"label must match exactly", acctX, "A", false, 0},
	{"unknown label finds nothing", "", "b", false, 0},
}

func declaring(capacity int) hostdispatcher.Heartbeat {
	h := hb(false)
	h.HostCapacity = &capacity
	return h
}

func checkGet(t *testing.T, repo Repository) {
	t.Helper()
	ctx := context.Background()
	for _, c := range getExactCases {
		rec, found, err := repo.Get(ctx, c.account, c.label)
		if err != nil {
			t.Fatalf("%s: Get(%q, %q): %v", c.name, c.account, c.label, err)
		}
		gotCap := 0
		if found && rec.Heartbeat.HostCapacity != nil {
			gotCap = *rec.Heartbeat.HostCapacity
		}
		if found != c.wantFound || gotCap != c.wantCap || (found && (rec.AccountID != c.account || rec.HostLabel != c.label)) {
			t.Errorf("%s: Get(%q, %q) = found %v, %s/%s host_capacity %d; want found %v host_capacity %d",
				c.name, c.account, c.label, found, rec.AccountID, rec.HostLabel, gotCap, c.wantFound, c.wantCap)
		}
	}
	if _, _, err := repo.Get(ctx, "not-a-uuid", "a"); err == nil {
		t.Error("Get(non-UUID account) = nil error, want a refusal before the store")
	}
}

// TestPostgres_GetExactAccount (pgtest) pins Get's EXACT account match (ADR-094
// A5 item 7): an untenanted and a tenanted row share one label, and each
// caller sees only the row in its own partition; with only the untenanted row
// present a tenanted caller finds nothing. Counterfactual run by hand:
// replacing getSQL's predicate with listSQL's `(account_id IS NULL OR
// account_id = $1)` went RED (acctY found the untenanted row).
func TestPostgres_GetExactAccount(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	if err := repo.Upsert(ctx, Record{HostLabel: "a", Heartbeat: declaring(1), ReceivedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := repo.Get(ctx, acctX, "a"); err != nil || found {
		t.Fatalf("only the untenanted row present: Get(acctX) found=%v (%v), want nothing", found, err)
	}
	if err := repo.Upsert(ctx, Record{AccountID: acctX, HostLabel: "a", TokenID: "tok-x", Heartbeat: declaring(2), ReceivedAt: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	checkGet(t, repo)
	rec, _, err := repo.Get(ctx, acctX, "a")
	if err != nil || rec.TokenID != "tok-x" || !rec.ReceivedAt.Equal(t0.Add(time.Second)) || len(rec.Heartbeat.RunRefusals) != 1 {
		t.Errorf("Get(acctX, a) = %+v (%v), want the full stored record", rec, err)
	}
}

// TestPostgres_DroppedDeclarationRoundTrip (pgtest): the server-only
// dropped_declaration persists in the payload and reads back through both Get
// and List, while a row without one reads back empty. Counterfactual run by
// hand: dropping storedPayload's DroppedDeclaration field went RED.
func TestPostgres_DroppedDeclarationRoundTrip(t *testing.T) {
	pool := pgtest.NewPool(t)
	repo := NewPostgresRepository(pool)
	ctx := context.Background()
	dropped := Record{HostLabel: "a", Heartbeat: hb(false), DroppedDeclaration: hostdispatcher.DeclarationFieldHostImplementLimit, ReceivedAt: t0}
	if err := repo.Upsert(ctx, dropped); err != nil {
		t.Fatal(err)
	}
	if err := repo.Upsert(ctx, Record{HostLabel: "b", Heartbeat: declaring(4), ReceivedAt: t0}); err != nil {
		t.Fatal(err)
	}
	rec, found, err := repo.Get(ctx, "", "a")
	if err != nil || !found || rec.DroppedDeclaration != hostdispatcher.DeclarationFieldHostImplementLimit {
		t.Fatalf("Get(a).DroppedDeclaration = %q (found %v, %v), want %q", rec.DroppedDeclaration, found, err, hostdispatcher.DeclarationFieldHostImplementLimit)
	}
	if rec.Heartbeat.HostCapacity != nil || rec.Heartbeat.HostImplementLimit != nil {
		t.Errorf("Get(a) declaration = %v/%v, want both absent", rec.Heartbeat.HostCapacity, rec.Heartbeat.HostImplementLimit)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT payload->>'dropped_declaration' FROM host_dispatcher_heartbeats WHERE host_label = 'a'`).Scan(&raw); err != nil ||
		raw != hostdispatcher.DeclarationFieldHostImplementLimit {
		t.Errorf("payload dropped_declaration column = %q (%v), want it stored at the payload's top level", raw, err)
	}
	all, err := repo.List(ctx, "", 10)
	if err != nil || len(all) != 2 {
		t.Fatalf("List = %d rows (%v), want 2", len(all), err)
	}
	for _, r := range all {
		want := ""
		if r.HostLabel == "a" {
			want = hostdispatcher.DeclarationFieldHostImplementLimit
		}
		if r.DroppedDeclaration != want {
			t.Errorf("List %s DroppedDeclaration = %q, want %q", r.HostLabel, r.DroppedDeclaration, want)
		}
		if r.HostLabel == "b" && (r.Heartbeat.HostCapacity == nil || *r.Heartbeat.HostCapacity != 4) {
			t.Errorf("List b host_capacity = %v, want 4 (the declaration round-trips)", r.Heartbeat.HostCapacity)
		}
	}
}

// TestMemory_Get pins the test double's Get to the Postgres table above, plus
// GetErr injection.
func TestMemory_Get(t *testing.T) {
	m := NewMemoryRepository()
	ctx := context.Background()
	if err := m.Upsert(ctx, Record{HostLabel: "a", Heartbeat: declaring(1), ReceivedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := m.Get(ctx, acctX, "a"); err != nil || found {
		t.Fatalf("only the untenanted row present: Get(acctX) found=%v (%v), want nothing", found, err)
	}
	if err := m.Upsert(ctx, Record{AccountID: acctX, HostLabel: "a", Heartbeat: declaring(2), DroppedDeclaration: "x", ReceivedAt: t0}); err != nil {
		t.Fatal(err)
	}
	checkGet(t, m)
	if rec, _, _ := m.Get(ctx, acctX, "a"); rec.DroppedDeclaration != "x" {
		t.Errorf("Get DroppedDeclaration = %q, want x", rec.DroppedDeclaration)
	}
	boom := errors.New("boom")
	m.GetErr = boom
	if _, found, err := m.Get(ctx, "", "a"); !errors.Is(err, boom) || found {
		t.Errorf("Get with GetErr = found %v, %v; want boom", found, err)
	}
}
