package hostdispatcher

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	hbRunA   = "0b6a3b8e-0d2f-4a38-9a66-1f5bfb3c2d10"
	hbRunB   = "1c7b4c9f-1e30-4b49-8b77-2a6cfc4d3e21"
	hbStageA = "2d8c5da0-2f41-4c5a-9c88-3b7d0d5e4f32"
)

var hbT0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func validHeartbeat() Heartbeat {
	return Heartbeat{
		IntervalSeconds: 30,
		StartedAt:       hbT0.Add(-time.Hour),
		LastTickAt:      hbT0,
		SpawnsToday:     1,
		SpawnsDay:       "2026-11-24",
		DailyCap:        4,
		InFlight:        1,
		RunRefusals: []RunRefusal{{
			RunID: hbRunA, StageID: hbStageA, Code: "path_outside_allowed_roots",
			Disposition: DispositionRefusing, FirstAt: hbT0, LastAt: hbT0,
		}},
	}
}

// TestHeartbeatValidate: each rejection row is invalid ONLY in its named
// field, so making Validate's body `return nil` turns every rejection row RED
// (counterfactual run by hand; recorded in the PR Notes).
func TestHeartbeatValidate(t *testing.T) {
	if err := validHeartbeat().Validate(); err != nil {
		t.Fatalf("valid heartbeat: %v", err)
	}
	many := func(n int) []RunRefusal {
		out := make([]RunRefusal, n)
		for i := range out {
			out[i] = RunRefusal{RunID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Code: "x", Disposition: DispositionWaiting}
		}
		return out
	}
	for _, c := range []struct {
		name, field string
		mut         func(*Heartbeat)
	}{
		{"zero interval", "interval_seconds", func(h *Heartbeat) { h.IntervalSeconds = 0 }},
		{"negative interval", "interval_seconds", func(h *Heartbeat) { h.IntervalSeconds = -5 }},
		{"missing started_at", "started_at", func(h *Heartbeat) { h.StartedAt = time.Time{} }},
		{"missing last_tick_at", "last_tick_at", func(h *Heartbeat) { h.LastTickAt = time.Time{} }},
		{"257 run_refusals", "run_refusals", func(h *Heartbeat) { h.RunRefusals = many(MaxRunRefusals + 1) }},
		{"duplicate run_id", "run_refusals[1].run_id", func(h *Heartbeat) {
			h.RunRefusals = append(h.RunRefusals, RunRefusal{RunID: hbRunA, Code: "x", Disposition: DispositionWaiting})
		}},
		{"non-UUID run_id", "run_refusals[0].run_id", func(h *Heartbeat) { h.RunRefusals[0].RunID = "not-a-uuid" }},
		{"uppercase run_id", "run_refusals[0].run_id", func(h *Heartbeat) { h.RunRefusals[0].RunID = strings.ToUpper(hbRunA) }},
		{"non-UUID stage_id", "run_refusals[0].stage_id", func(h *Heartbeat) { h.RunRefusals[0].StageID = "stage-1" }},
		{"negative daily_cap", "daily_cap", func(h *Heartbeat) { h.DailyCap = -1 }},
		{"negative spawns_today", "spawns_today", func(h *Heartbeat) { h.SpawnsToday = -1 }},
		{"negative in_flight", "in_flight", func(h *Heartbeat) { h.InFlight = -1 }},
		{"bad spawns_day", "spawns_day", func(h *Heartbeat) { h.SpawnsDay = "2026-13-40" }},
	} {
		h := validHeartbeat()
		c.mut(&h)
		err := h.Validate()
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: Validate() = %v, want a *ValidationError", c.name, err)
			continue
		}
		if ve.Field != c.field {
			t.Errorf("%s: field = %q, want %q", c.name, ve.Field, c.field)
		}
	}
	// Exactly MaxRunRefusals entries pass; an empty stage_id passes.
	h := validHeartbeat()
	h.RunRefusals = many(MaxRunRefusals)
	if err := h.Validate(); err != nil {
		t.Errorf("%d entries: %v, want nil", MaxRunRefusals, err)
	}
	// Off-grammar entries are NOT structural errors (condition H3): Validate
	// admits them and DropInvalidRefusals removes them.
	h = validHeartbeat()
	h.RunRefusals[0].Code = "Has Space"
	h.RunRefusals[0].Disposition = "bogus"
	if err := h.Validate(); err != nil {
		t.Errorf("off-grammar entry: Validate() = %v, want nil (dropped at ingest, not refused)", err)
	}
}

// TestHeartbeatDropInvalidRefusals (#5489 condition H3): each off-grammar
// entry is dropped and counted; its valid sibling survives. Counterfactual
// run by hand: making RefusalValid `return true` kept every bad row, RED.
func TestHeartbeatDropInvalidRefusals(t *testing.T) {
	good := RunRefusal{RunID: hbRunA, Code: "budget_blocked", Disposition: DispositionBudgetBlocked, BudgetSource: BudgetSourceLiveSpec}
	for _, c := range []struct {
		name string
		bad  RunRefusal
	}{
		{"65-byte code", RunRefusal{Code: strings.Repeat("a", MaxCodeLen+1), Disposition: DispositionRefusing}},
		{"empty code", RunRefusal{Code: "", Disposition: DispositionRefusing}},
		{"uppercase code", RunRefusal{Code: "Path_Outside", Disposition: DispositionRefusing}},
		{"space in code", RunRefusal{Code: "path outside", Disposition: DispositionRefusing}},
		{"unknown disposition", RunRefusal{Code: "x", Disposition: "stuck"}},
		{"empty disposition", RunRefusal{Code: "x"}},
		{"budget_source outside the set", RunRefusal{Code: "budget_blocked", Disposition: DispositionBudgetBlocked, BudgetSource: "guess"}},
		{"budget_source on a refusing entry", RunRefusal{Code: "budget_check_failed", Disposition: DispositionRefusing, BudgetSource: BudgetSourceLiveSpec}},
	} {
		c.bad.RunID = hbRunB
		h := validHeartbeat()
		h.RunRefusals = []RunRefusal{good, c.bad}
		if n := h.DropInvalidRefusals(); n != 1 {
			t.Errorf("%s: dropped = %d, want 1", c.name, n)
		}
		if len(h.RunRefusals) != 1 || h.RunRefusals[0] != good {
			t.Errorf("%s: kept %+v, want only the valid entry", c.name, h.RunRefusals)
		}
	}
	// Every disposition and budget_source in the closed sets survives.
	h := validHeartbeat()
	h.RunRefusals = nil
	for i, d := range []Disposition{DispositionWaiting, DispositionInFlight, DispositionHeld, DispositionBudgetBlocked,
		DispositionPaused, DispositionCapped, DispositionRefusing} {
		h.RunRefusals = append(h.RunRefusals, RunRefusal{RunID: fmt.Sprintf("00000000-0000-4000-8000-%012x", i), Code: "cap_reached:daily.x-1", Disposition: d})
	}
	for i, src := range []string{BudgetSourceLiveSpec, BudgetSourceAdmissionSpec, BudgetSourceAdmissionSpecFallback} {
		h.RunRefusals = append(h.RunRefusals, RunRefusal{RunID: fmt.Sprintf("00000000-0000-4000-9000-%012x", i), Code: strings.Repeat("b", MaxCodeLen), Disposition: DispositionBudgetBlocked, BudgetSource: src})
	}
	if n := h.DropInvalidRefusals(); n != 0 {
		t.Errorf("closed-set entries: dropped %d, want 0", n)
	}
}

// TestHeartbeatBoundFields: a 1,100-byte detail with control runes and
// invalid UTF-8 is bounded, not refused.
func TestHeartbeatBoundFields(t *testing.T) {
	h := validHeartbeat()
	h.RunRefusals[0].Detail = "a\x00b\nc\xff" + strings.Repeat("é", 550)
	h.TickFailureCode = "ledger_read_failed\t"
	if err := h.Validate(); err != nil {
		t.Fatalf("long detail: Validate() = %v, want nil", err)
	}
	h.BoundFields()
	d := h.RunRefusals[0].Detail
	if len(d) > MaxTextLen || !utf8.ValidString(d) {
		t.Errorf("bounded detail = %d bytes valid=%v, want <= %d valid UTF-8", len(d), utf8.ValidString(d), MaxTextLen)
	}
	if !strings.HasPrefix(d, "abcé") || strings.ContainsAny(d, "\x00\n") {
		t.Errorf("bounded detail = %q, want control runes and invalid bytes dropped", d[:12])
	}
	if h.TickFailureCode != "ledger_read_failed" {
		t.Errorf("tick_failure_code = %q, want the tab dropped", h.TickFailureCode)
	}
	if got := BoundText(strings.Repeat("x", 299) + "é"); got != strings.Repeat("x", 299) {
		t.Errorf("BoundText cut mid-rune: %q", got[290:])
	}
}

// TestHeartbeatStaleAfter pins the reader clamp. Counterfactual run by hand:
// deleting the clamp turned the 1e9 and 0.1 rows RED.
func TestHeartbeatStaleAfter(t *testing.T) {
	for _, c := range []struct {
		in   float64
		want time.Duration
	}{
		{0, 90 * time.Second},
		{-5, 90 * time.Second},
		{30, 90 * time.Second},
		{45, 135 * time.Second},
		{1e9, 900 * time.Second},
		{0.1, 3 * time.Second},
	} {
		if got := StaleAfter(c.in); got != c.want {
			t.Errorf("StaleAfter(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	if FutureSkew != 60*time.Second {
		t.Errorf("FutureSkew = %v, want 60s (the status.json reader's hostDispatchStatusFutureSkew)", FutureSkew)
	}
}
