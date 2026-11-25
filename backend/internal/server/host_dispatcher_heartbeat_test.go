package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/dispatcherheartbeat"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hostdispatcher"
)

// Host-dispatcher heartbeat ingest tests (#5489). Each control's hand-run
// counterfactual is recorded in the PR Notes and the sidecar; the short form
// sits on each test.

const (
	hbPath    = "/v0/host-dispatchers/heartbeat"
	hbE2EHost = "host-dispatcher/e2e-host"
	hbRunID   = "0b6a3b8e-0d2f-4a38-9a66-1f5bfb3c2d10"
	hbRunID2  = "1c7b4c9f-1e30-4b49-8b77-2a6cfc4d3e21"
	hbAccount = "6f1c2d3e-4a5b-4c6d-8e7f-90a1b2c3d4e5"
)

var hbNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// newHBFixture is the identity fixture with a MemoryRepository heartbeat store
// and a fixed server clock.
func newHBFixture(t *testing.T) (*hdFixture, *dispatcherheartbeat.MemoryRepository) {
	t.Helper()
	f := newHDFixture(t)
	mem := dispatcherheartbeat.NewMemoryRepository()
	f.s.cfg.HostDispatcherHeartbeatRepo = mem
	f.s.nowFunc = func() time.Time { return hbNow }
	return f, mem
}

func hbBody(t *testing.T, mut func(*hostdispatcher.Heartbeat)) string {
	t.Helper()
	hb := hostdispatcher.Heartbeat{
		IntervalSeconds: 30,
		StartedAt:       hbNow.Add(-time.Hour),
		LastTickAt:      hbNow.Add(-2 * time.Second),
		Paused:          true,
		SpawnsToday:     1,
		SpawnsDay:       "2026-11-24",
		DailyCap:        3,
		RunRefusals: []hostdispatcher.RunRefusal{{
			RunID: hbRunID, Code: "path_outside_allowed_roots", Detail: "outside /srv",
			Disposition: hostdispatcher.DispositionRefusing, FirstAt: hbNow.Add(-time.Minute), LastAt: hbNow,
		}},
	}
	if mut != nil {
		mut(&hb)
	}
	b, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hbErrField(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	s, _ := env.Error.Details["field"].(string)
	return s
}

func hbDropped(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var resp struct {
		DroppedRefusals *int `json:"dropped_refusals"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.DroppedRefusals == nil {
		t.Fatalf("response %q carries no dropped_refusals: %v", w.Body.String(), err)
	}
	return *resp.DroppedRefusals
}

// TestHostDispatcherHeartbeat_RecordsDispatcherSubject: a dispatcher token's
// heartbeat lands under its subject label, untenanted, stamped with the
// server clock; a second POST replaces it.
func TestHostDispatcherHeartbeat_RecordsDispatcherSubject(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	w := f.do(http.MethodPost, hbPath, tok, hbBody(t, nil))
	if w.Code != http.StatusOK || hbDropped(t, w) != 0 {
		t.Fatalf("POST = %d %s; want 200 dropped_refusals 0", w.Code, w.Body.String())
	}
	recs := mem.Records()
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.HostLabel != "e2e-host" || rec.AccountID != "" || rec.TokenID == "" || !rec.ReceivedAt.Equal(hbNow) {
		t.Errorf("record = label %q account %q token %q received %v; want e2e-host, untenanted, a token id, %v",
			rec.HostLabel, rec.AccountID, rec.TokenID, rec.ReceivedAt, hbNow)
	}
	if !rec.Heartbeat.Paused || rec.Heartbeat.DailyCap != 3 || len(rec.Heartbeat.RunRefusals) != 1 ||
		rec.Heartbeat.RunRefusals[0].Detail != "outside /srv" {
		t.Errorf("payload did not round-trip: %+v", rec.Heartbeat)
	}

	f.s.nowFunc = func() time.Time { return hbNow.Add(30 * time.Second) }
	w = f.do(http.MethodPost, hbPath, tok, hbBody(t, func(h *hostdispatcher.Heartbeat) { h.Paused = false; h.RunRefusals = nil }))
	if w.Code != http.StatusOK {
		t.Fatalf("second POST = %d %s", w.Code, w.Body.String())
	}
	recs = mem.Records()
	if len(recs) != 1 || recs[0].Heartbeat.Paused || len(recs[0].Heartbeat.RunRefusals) != 0 ||
		!recs[0].ReceivedAt.Equal(hbNow.Add(30*time.Second)) {
		t.Errorf("after second POST = %+v; want one record carrying the newest payload", recs)
	}
}

// TestHostDispatcherHeartbeat_RecordsTokenAccount: an account-bound
// dispatcher token's heartbeat lands in that account. Counterfactual run by
// hand: replacing the AccountID assignment with "" left the record
// untenanted, RED.
func TestHostDispatcherHeartbeat_RecordsTokenAccount(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	f.tokens.mu.Lock()
	for _, row := range f.tokens.rows {
		if row.Subject == hbE2EHost {
			row.AccountID = hbAccount
		}
	}
	f.tokens.mu.Unlock()
	if w := f.do(http.MethodPost, hbPath, tok, hbBody(t, nil)); w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	recs := mem.Records()
	if len(recs) != 1 || recs[0].AccountID != hbAccount {
		t.Fatalf("records = %+v; want one record in account %s", recs, hbAccount)
	}
}

// TestHostDispatcherHeartbeat_RefusesNonDispatcherSubject: an operator token
// holding write:runs passes bearerAuth and the scope gate, and the memory
// store has no label CHECK, so the subject refusal is the only control in the
// path. Counterfactual run by hand: making the refusal fall through stored a
// record with an empty label, RED on the record count.
func TestHostDispatcherHeartbeat_RefusesNonDispatcherSubject(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint("github:operator", "write:runs", "read:runs")
	w := f.do(http.MethodPost, hbPath, tok, hbBody(t, nil))
	if w.Code != http.StatusForbidden || hdErrCode(t, w) != "host_dispatcher_subject_required" {
		t.Errorf("operator POST = %d %s; want 403 host_dispatcher_subject_required", w.Code, w.Body.String())
	}
	if n := len(mem.Records()); n != 0 {
		t.Errorf("records after a refused operator POST = %d, want 0", n)
	}
}

// TestHostDispatcherHeartbeat_RefusesUnauthenticated: no bearer answers 401.
// Counterfactual run by hand: deleting the requireWriteScope call let the
// anonymous identity reach the subject check (403), RED on the status.
func TestHostDispatcherHeartbeat_RefusesUnauthenticated(t *testing.T) {
	f, mem := newHBFixture(t)
	w := f.do(http.MethodPost, hbPath, "", hbBody(t, nil))
	if w.Code != http.StatusUnauthorized || hdErrCode(t, w) != "authentication_required" {
		t.Errorf("anonymous POST = %d %s; want 401 authentication_required", w.Code, w.Body.String())
	}
	if n := len(mem.Records()); n != 0 {
		t.Errorf("records after an anonymous POST = %d, want 0", n)
	}
}

// TestHostDispatcherHeartbeat_MissingScope: a dispatcher token minted without
// write:runs answers 403 insufficient_scope. Its subject is valid, so
// deleting the requireWriteScope call stores a record, RED (run by hand).
func TestHostDispatcherHeartbeat_MissingScope(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.ScopeReadRuns, hostdispatcher.ScopeWriteStages)
	w := f.do(http.MethodPost, hbPath, tok, hbBody(t, nil))
	if w.Code != http.StatusForbidden || hdErrCode(t, w) != "insufficient_scope" {
		t.Errorf("POST without write:runs = %d %s; want 403 insufficient_scope", w.Code, w.Body.String())
	}
	if n := len(mem.Records()); n != 0 {
		t.Errorf("records = %d, want 0", n)
	}
}

// TestHostDispatcherHeartbeat_RejectsInvalidBody: structural errors answer 400
// validation_failed naming the field, and store nothing.
func TestHostDispatcherHeartbeat_RejectsInvalidBody(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	withKey := func(key string) string {
		b := hbBody(t, nil)
		return `{"` + key + `":"x",` + b[1:]
	}
	for _, c := range []struct{ name, body, field string }{
		{"unknown host key", withKey("host"), "body"},
		{"unknown account_id key", withKey("account_id"), "body"},
		{"malformed JSON", `{"interval_seconds":`, "body"},
		{"missing interval", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.IntervalSeconds = 0 }), "interval_seconds"},
		{"negative interval", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.IntervalSeconds = -1 }), "interval_seconds"},
		{"missing started_at", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.StartedAt = time.Time{} }), "started_at"},
		{"missing last_tick_at", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.LastTickAt = time.Time{} }), "last_tick_at"},
		{"negative daily_cap", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.DailyCap = -1 }), "daily_cap"},
		{"negative spawns_today", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.SpawnsToday = -1 }), "spawns_today"},
		{"negative in_flight", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.InFlight = -1 }), "in_flight"},
		{"bad spawns_day", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.SpawnsDay = "2026-13-40" }), "spawns_day"},
		{"non-UUID run_id", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.RunRefusals[0].RunID = "run-1" }), "run_refusals[0].run_id"},
		{"non-UUID stage_id", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.RunRefusals[0].StageID = "stage-1" }), "run_refusals[0].stage_id"},
		{"duplicate run_id", hbBody(t, func(h *hostdispatcher.Heartbeat) { h.RunRefusals = append(h.RunRefusals, h.RunRefusals[0]) }), "run_refusals[1].run_id"},
		{"257 run_refusals", hbBody(t, func(h *hostdispatcher.Heartbeat) {
			h.RunRefusals = nil
			for i := 0; i <= hostdispatcher.MaxRunRefusals; i++ {
				h.RunRefusals = append(h.RunRefusals, hostdispatcher.RunRefusal{
					RunID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Code: "x", Disposition: hostdispatcher.DispositionWaiting})
			}
		}), "run_refusals"},
	} {
		w := f.do(http.MethodPost, hbPath, tok, c.body)
		if w.Code != http.StatusBadRequest || hdErrCode(t, w) != "validation_failed" || hbErrField(t, w) != c.field {
			t.Errorf("%s: %d %s; want 400 validation_failed field %q", c.name, w.Code, w.Body.String(), c.field)
		}
		if n := len(mem.Records()); n != 0 {
			t.Fatalf("%s: records = %d, want 0", c.name, n)
		}
	}
}

// TestHostDispatcherHeartbeat_DropsOffGrammarRefusal (#5489 condition H3): one
// valid and one off-grammar entry: the heartbeat is stored with the valid
// entry and the response reports dropped_refusals 1. Counterfactual run by
// hand: deleting the DropInvalidRefusals call stored both entries and reported
// 0, RED.
func TestHostDispatcherHeartbeat_DropsOffGrammarRefusal(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	body := hbBody(t, func(h *hostdispatcher.Heartbeat) {
		h.RunRefusals = append(h.RunRefusals, hostdispatcher.RunRefusal{
			RunID: hbRunID2, Code: "Not A Code", Disposition: hostdispatcher.DispositionRefusing,
			FirstAt: hbNow, LastAt: hbNow,
		})
	})
	w := f.do(http.MethodPost, hbPath, tok, body)
	if w.Code != http.StatusOK || hbDropped(t, w) != 1 {
		t.Fatalf("POST = %d %s; want 200 dropped_refusals 1", w.Code, w.Body.String())
	}
	recs := mem.Records()
	if len(recs) != 1 || len(recs[0].Heartbeat.RunRefusals) != 1 || recs[0].Heartbeat.RunRefusals[0].RunID != hbRunID {
		t.Fatalf("records = %+v; want the heartbeat stored with only the valid entry", recs)
	}
	if !recs[0].Heartbeat.Paused {
		t.Error("the rest of the heartbeat was not persisted")
	}
	// A bad disposition and a stray budget_source are dropped the same way.
	body = hbBody(t, func(h *hostdispatcher.Heartbeat) {
		h.RunRefusals = append(h.RunRefusals,
			hostdispatcher.RunRefusal{RunID: hbRunID2, Code: "x", Disposition: "stuck"},
			hostdispatcher.RunRefusal{RunID: "2d8c5da0-2f41-4c5a-9c88-3b7d0d5e4f32", Code: "budget_check_failed",
				Disposition: hostdispatcher.DispositionRefusing, BudgetSource: hostdispatcher.BudgetSourceLiveSpec})
	})
	if w := f.do(http.MethodPost, hbPath, tok, body); w.Code != http.StatusOK || hbDropped(t, w) != 2 {
		t.Errorf("POST with two bad entries = %d %s; want 200 dropped_refusals 2", w.Code, w.Body.String())
	}
}

// TestHostDispatcherHeartbeat_BoundsText: a long, control-laden detail is
// bounded at ingest, not refused.
func TestHostDispatcherHeartbeat_BoundsText(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	body := hbBody(t, func(h *hostdispatcher.Heartbeat) {
		h.RunRefusals[0].Detail = "line\none\x00" + strings.Repeat("d", 1100)
		h.TickFailureCode = "ledger_read_failed\n"
	})
	if w := f.do(http.MethodPost, hbPath, tok, body); w.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", w.Code, w.Body.String())
	}
	hb := mem.Records()[0].Heartbeat
	if d := hb.RunRefusals[0].Detail; len(d) != hostdispatcher.MaxTextLen || strings.ContainsAny(d, "\n\x00") {
		t.Errorf("stored detail = %d bytes %q...; want %d bytes, control runes dropped", len(d), d[:12], hostdispatcher.MaxTextLen)
	}
	if hb.TickFailureCode != "ledger_read_failed" {
		t.Errorf("tick_failure_code = %q, want bounded", hb.TickFailureCode)
	}
}

// TestHostDispatcherHeartbeat_BodyTooLarge: a VALID body over 256 KiB (256
// entries, each with a 1,100-byte detail) answers 400 field body. Without the
// cap it decodes, validates and is stored with bounded details, so deleting
// the MaxBytesReader stores a record, RED (run by hand).
func TestHostDispatcherHeartbeat_BodyTooLarge(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	body := hbBody(t, func(h *hostdispatcher.Heartbeat) {
		h.RunRefusals = nil
		for i := 0; i < hostdispatcher.MaxRunRefusals; i++ {
			h.RunRefusals = append(h.RunRefusals, hostdispatcher.RunRefusal{
				RunID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Code: "path_outside_allowed_roots",
				Detail: strings.Repeat("d", 1100), Disposition: hostdispatcher.DispositionRefusing})
		}
	})
	if len(body) <= hostDispatcherHeartbeatMaxBodyBytes {
		t.Fatalf("fixture body is %d bytes; it must exceed the %d-byte cap", len(body), hostDispatcherHeartbeatMaxBodyBytes)
	}
	w := f.do(http.MethodPost, hbPath, tok, body)
	if w.Code != http.StatusBadRequest || hdErrCode(t, w) != "validation_failed" || hbErrField(t, w) != "body" {
		t.Errorf("oversized POST = %d %s; want 400 validation_failed field body", w.Code, w.Body.String())
	}
	if n := len(mem.Records()); n != 0 {
		t.Errorf("records after an oversized POST = %d, want 0", n)
	}
}

// TestHostDispatcherHeartbeat_Unwired: a nil repository answers 503
// not_configured, before any auth or body work.
func TestHostDispatcherHeartbeat_Unwired(t *testing.T) {
	f := newHDFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	w := f.do(http.MethodPost, hbPath, tok, hbBody(t, nil))
	if w.Code != http.StatusServiceUnavailable || hdErrCode(t, w) != "not_configured" {
		t.Errorf("unwired POST = %d %s; want 503 not_configured", w.Code, w.Body.String())
	}
}

// TestHostDispatcherHeartbeat_StoreErrors: a database-unavailable store error
// answers 503 service_unavailable; any other answers 500 internal_error.
func TestHostDispatcherHeartbeat_StoreErrors(t *testing.T) {
	f, mem := newHBFixture(t)
	tok := f.mint(hbE2EHost, hostdispatcher.Scopes()...)
	mem.UpsertErr = fmt.Errorf("wrapped: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
	if w := f.do(http.MethodPost, hbPath, tok, hbBody(t, nil)); w.Code != http.StatusServiceUnavailable || hdErrCode(t, w) != "service_unavailable" {
		t.Errorf("unavailable store = %d %s; want 503 service_unavailable", w.Code, w.Body.String())
	}
	mem.UpsertErr = errors.New("constraint violated")
	w := f.do(http.MethodPost, hbPath, tok, hbBody(t, nil))
	if w.Code != http.StatusInternalServerError || hdErrCode(t, w) != "internal_error" {
		t.Errorf("failing store = %d %s; want 500 internal_error", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "constraint violated") {
		t.Errorf("500 body leaks the store cause: %s", w.Body.String())
	}
}
