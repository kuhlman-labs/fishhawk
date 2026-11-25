package hostdispatcher

import (
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The server heartbeat (#5489, ADR-104 Amendment A2): each tick,
// `fishhawk-mcp host-dispatch` POSTs a Heartbeat to HeartbeatRoute and the
// server upserts it, keyed by the token's account and the subject's host
// label, so the campaign awaiting block and the Web UI can see dispatcher
// state. The wire type lives here so the dispatcher and the server share one
// definition. The body carries no host and no account: the server takes both
// from the bearer token. Long-form contract: README.md § Server heartbeat.

// HeartbeatRoute is the heartbeat write path (POST).
const HeartbeatRoute = "/v0/host-dispatchers/heartbeat"

// HeartbeatResponse is the 200 body: how many run_refusals entries the server
// dropped at ingest for an off-grammar code, disposition or budget_source
// (#5489 condition H3). The rest of the heartbeat was stored. The dispatcher
// logs a non-zero count, because a dropped entry's run reads live on the
// server while the dispatcher refuses it.
type HeartbeatResponse struct {
	DroppedRefusals int `json:"dropped_refusals"`
}

// TickFailureCodeInvalid replaces an off-grammar tick_failure_code at ingest
// (BoundFields): the tick still reads as failing, but no dispatcher-authored
// free text reaches a reader through this field.
const TickFailureCodeInvalid = "invalid_tick_failure_code"

// Heartbeat bounds. MaxRunRefusals caps run_refusals (a longer list is a 400);
// MaxCodeLen bounds a refusal code; MaxTextLen is the BoundText cut, the same
// 300 bytes as the dispatcher's own status.json bound.
const (
	MaxRunRefusals = 256
	MaxCodeLen     = 64
	MaxTextLen     = 300
)

// Staleness rules, mirroring the status.json reader in
// backend/internal/mcpserver/host_dispatcher_loop.go
// (hostDispatchStatusStaleTicks, hostDispatchStatusMinIntervalSeconds,
// hostDispatchStatusMaxIntervalSeconds, hostDispatchDefaultInterval and
// hostDispatchStatusFutureSkew).
const (
	StaleTicks         = 3
	MinIntervalSeconds = 1
	MaxIntervalSeconds = 300
	DefaultInterval    = 30 * time.Second
	// FutureSkew: a heartbeat received more than this far ahead of the
	// reader's clock reads stale.
	FutureSkew = 60 * time.Second
)

// Disposition is the dispatcher-assigned reading of one run refusal: what
// the dispatcher will do about the run. It is a closed set.
type Disposition string

// The closed disposition set.
const (
	DispositionWaiting       Disposition = "waiting"
	DispositionInFlight      Disposition = "in_flight"
	DispositionHeld          Disposition = "held"
	DispositionBudgetBlocked Disposition = "budget_blocked"
	DispositionPaused        Disposition = "paused"
	DispositionCapped        Disposition = "capped"
	DispositionRefusing      Disposition = "refusing"
)

// Valid reports whether d is in the closed set.
func (d Disposition) Valid() bool {
	switch d {
	case DispositionWaiting, DispositionInFlight, DispositionHeld, DispositionBudgetBlocked,
		DispositionPaused, DispositionCapped, DispositionRefusing:
		return true
	}
	return false
}

// The closed budget_source set (the server's dispatchBudgetSource* tokens,
// #5473). Legal only on a budget_blocked entry.
const (
	BudgetSourceLiveSpec              = "live_spec"
	BudgetSourceAdmissionSpec         = "admission_spec"
	BudgetSourceAdmissionSpecFallback = "admission_spec_fallback"
)

// ValidBudgetSource reports whether s is in the closed budget_source set.
func ValidBudgetSource(s string) bool {
	switch s {
	case BudgetSourceLiveSpec, BudgetSourceAdmissionSpec, BudgetSourceAdmissionSpecFallback:
		return true
	}
	return false
}

// Heartbeat is one dispatcher tick's snapshot.
type Heartbeat struct {
	IntervalSeconds    float64   `json:"interval_seconds"`
	StartedAt          time.Time `json:"started_at"`
	LastTickAt         time.Time `json:"last_tick_at"`
	LastCompleteTickAt time.Time `json:"last_complete_tick_at,omitzero"`
	Paused             bool      `json:"paused"`
	// TickFailureCode is the current tick's failing early return, absent when
	// the tick did not fail. Ingest holds it to the refusal-code grammar
	// (BoundFields).
	TickFailureCode string `json:"tick_failure_code,omitempty"`
	// DailyCap is the opt-in daily spawn limit; absent (0) when none is set.
	DailyCap    int `json:"daily_cap,omitempty"`
	SpawnsToday int `json:"spawns_today"`
	// SpawnsDay is the UTC date (YYYY-MM-DD) SpawnsToday counts.
	SpawnsDay   string       `json:"spawns_day,omitempty"`
	InFlight    int          `json:"in_flight"`
	RunRefusals []RunRefusal `json:"run_refusals"`
}

// RunRefusal is one candidate run's latest refusal.
type RunRefusal struct {
	RunID       string      `json:"run_id"`
	StageID     string      `json:"stage_id,omitempty"`
	Code        string      `json:"code"`
	Detail      string      `json:"detail,omitempty"`
	Disposition Disposition `json:"disposition"`
	// BudgetSource names which budget verdict blocked the run; legal only on
	// a budget_blocked entry.
	BudgetSource string    `json:"budget_source,omitempty"`
	FirstAt      time.Time `json:"first_at"`
	LastAt       time.Time `json:"last_at"`
}

// ValidationError names the field a structural check refused.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return "hostdispatcher: heartbeat " + e.Field + " " + e.Reason
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// Validate applies the STRUCTURAL checks a heartbeat must pass as a whole: a
// positive interval_seconds, non-zero started_at and last_tick_at, no negative
// count, a parseable spawns_day, at most MaxRunRefusals entries, and per entry
// a canonical run_id (unique across the list) and an empty or canonical
// stage_id. A failure is a 400. An entry whose code, disposition or
// budget_source is off-grammar is NOT a structural error: DropInvalidRefusals
// removes it so one bad entry cannot blank a dispatcher (#5489 condition H3).
func (h Heartbeat) Validate() error {
	if !(h.IntervalSeconds > 0) || math.IsInf(h.IntervalSeconds, 0) {
		return invalid("interval_seconds", "must be a positive number of seconds")
	}
	if h.StartedAt.IsZero() {
		return invalid("started_at", "is required")
	}
	if h.LastTickAt.IsZero() {
		return invalid("last_tick_at", "is required")
	}
	if h.DailyCap < 0 {
		return invalid("daily_cap", "must not be negative")
	}
	if h.SpawnsToday < 0 {
		return invalid("spawns_today", "must not be negative")
	}
	if h.InFlight < 0 {
		return invalid("in_flight", "must not be negative")
	}
	if h.SpawnsDay != "" {
		if _, err := time.Parse(time.DateOnly, h.SpawnsDay); err != nil {
			return invalid("spawns_day", "must be a YYYY-MM-DD date")
		}
	}
	if len(h.RunRefusals) > MaxRunRefusals {
		return invalid("run_refusals", "holds %d entries; at most %d are allowed", len(h.RunRefusals), MaxRunRefusals)
	}
	seen := make(map[string]bool, len(h.RunRefusals))
	for i, ref := range h.RunRefusals {
		if !isCanonicalUUID(ref.RunID) {
			return invalid(fmt.Sprintf("run_refusals[%d].run_id", i), "must be a lowercase 8-4-4-4-12 hex UUID")
		}
		if seen[ref.RunID] {
			return invalid(fmt.Sprintf("run_refusals[%d].run_id", i), "duplicates an earlier entry")
		}
		seen[ref.RunID] = true
		if ref.StageID != "" && !isCanonicalUUID(ref.StageID) {
			return invalid(fmt.Sprintf("run_refusals[%d].stage_id", i), "must be a lowercase 8-4-4-4-12 hex UUID")
		}
	}
	return nil
}

// RefusalValid reports whether ref's code, disposition and budget_source are
// on-grammar: a 1..MaxCodeLen byte code from [a-z0-9_:.-], a disposition in
// the closed set, and a budget_source that is empty or, on a budget_blocked
// entry only, in the closed set.
func RefusalValid(ref RunRefusal) bool {
	if !validCode(ref.Code) || !ref.Disposition.Valid() {
		return false
	}
	if ref.BudgetSource != "" {
		if ref.Disposition != DispositionBudgetBlocked || !ValidBudgetSource(ref.BudgetSource) {
			return false
		}
	}
	return true
}

// DropInvalidRefusals removes every entry RefusalValid refuses, keeping the
// rest in order, and returns how many it dropped (#5489 condition H3).
func (h *Heartbeat) DropInvalidRefusals() int {
	kept := h.RunRefusals[:0]
	dropped := 0
	for _, ref := range h.RunRefusals {
		if !RefusalValid(ref) {
			dropped++
			continue
		}
		kept = append(kept, ref)
	}
	h.RunRefusals = kept
	return dropped
}

// BoundFields bounds the dispatcher-authored fields instead of refusing them,
// so one bad value cannot blank a dispatcher. Each refusal's detail and the
// tick_failure_code get BoundText. A non-empty tick_failure_code that is then
// still outside the refusal-code grammar (1..MaxCodeLen bytes of
// [a-z0-9_:.-]) becomes TickFailureCodeInvalid: the field is copied to every
// item the record decides, so it carries a code, never free text.
func (h *Heartbeat) BoundFields() {
	if h.TickFailureCode = BoundText(h.TickFailureCode); h.TickFailureCode != "" && !validCode(h.TickFailureCode) {
		h.TickFailureCode = TickFailureCodeInvalid
	}
	for i := range h.RunRefusals {
		h.RunRefusals[i].Detail = BoundText(h.RunRefusals[i].Detail)
	}
}

// BoundText drops invalid UTF-8 and control runes and cuts the result at
// MaxTextLen bytes on a rune boundary: the dispatcher's boundHostDispatchText
// rule (backend/internal/mcpserver/host_dispatcher_loop.go).
func BoundText(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= MaxTextLen {
		return out
	}
	cut := MaxTextLen
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut]
}

// StaleAfter is the freshness window for a heartbeat whose interval_seconds is
// intervalSeconds: StaleTicks intervals, with the interval clamped to
// [MinIntervalSeconds, MaxIntervalSeconds] (on the float, before the Duration
// conversion) or DefaultInterval when it is absent or not positive. A huge
// interval therefore cannot keep a dead dispatcher fresh.
func StaleAfter(intervalSeconds float64) time.Duration {
	interval := DefaultInterval
	if intervalSeconds > 0 {
		secs := min(max(intervalSeconds, MinIntervalSeconds), MaxIntervalSeconds)
		interval = time.Duration(secs * float64(time.Second))
	}
	return StaleTicks * interval
}

func validCode(code string) bool {
	if code == "" || len(code) > MaxCodeLen {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == ':', c == '.', c == '-':
		default:
			return false
		}
	}
	return true
}

// isCanonicalUUID reports whether s is a lowercase 8-4-4-4-12 hex UUID, the
// form the server mints and compares run and stage ids in.
func isCanonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}
