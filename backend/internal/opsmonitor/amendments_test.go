package opsmonitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// --- fixtures -----------------------------------------------------------------

// fakeAmendments is a fixed-row AmendmentReader recording its arguments.
type fakeAmendments struct {
	rows []scopeamendment.PendingWithStage
	err  error

	mu        sync.Mutex
	lookbacks []time.Duration
	limits    []int
}

func (f *fakeAmendments) ListPendingWithStage(_ context.Context, lookback time.Duration, limit int) ([]scopeamendment.PendingWithStage, error) {
	f.mu.Lock()
	f.lookbacks = append(f.lookbacks, lookback)
	f.limits = append(f.limits, limit)
	f.mu.Unlock()
	return f.rows, f.err
}

// amendObservedAt is the fixture's database now(): every row's age is exact.
var amendObservedAt = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func amendRow(age time.Duration, stageState string) scopeamendment.PendingWithStage {
	state := stageState
	return scopeamendment.PendingWithStage{
		ID:          uuid.New(),
		RunID:       uuid.New(),
		StageID:     uuid.New(),
		Status:      scopeamendment.StatusPending,
		RequestedAt: amendObservedAt.Add(-age),
		StageState:  &state,
		ObservedAt:  amendObservedAt,
	}
}

func hiddenRow(age time.Duration) scopeamendment.PendingWithStage {
	r := amendRow(age, "running")
	r.StageState = nil
	return r
}

func windowProbe(reader AmendmentReader, warnLead time.Duration) AmendmentWindowProbe {
	return AmendmentWindowProbe{Amendments: reader, Window: 15 * time.Minute, WarnLead: warnLead, Lookback: 7 * 24 * time.Hour}
}

func failingOf(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.Status == StatusFailing {
			out = append(out, r)
		}
	}
	return out
}

func unknownsOf(rs []Result) []Result {
	var out []Result
	for _, r := range rs {
		if r.Status == StatusUnknown {
			out = append(out, r)
		}
	}
	return out
}

// --- amendment_window ---------------------------------------------------------

// TestAmendmentWindowProbe_FiresAfterWindowMinusWarnLead: window 15m, lead
// 10m: a 6m-old row on a running stage fails with the structured Fields; a
// row aged exactly 5m does NOT fire (strictly exceeds).
func TestAmendmentWindowProbe_FiresAfterWindowMinusWarnLead(t *testing.T) {
	due, boundary := amendRow(6*time.Minute, "running"), amendRow(5*time.Minute, "running")
	reader := &fakeAmendments{rows: []scopeamendment.PendingWithStage{due, boundary}}
	got := windowProbe(reader, 10*time.Minute).Check(context.Background())
	if len(got) != 1 || got[0].Status != StatusFailing {
		t.Fatalf("results = %+v, want exactly one failing result (the 6m row)", got)
	}
	r := got[0]
	if r.Probe != "amendment_window" || r.Condition != ConditionAmendmentWindowClosing || r.Subject != due.ID.String() {
		t.Errorf("result = %+v, want amendment_window_closing on subject %s", r, due.ID)
	}
	for k, want := range map[string]any{
		"amendment_id":           due.ID.String(),
		"run_id":                 due.RunID.String(),
		"stage_id":               due.StageID.String(),
		"stage_state":            "running",
		"requested_at":           due.RequestedAt.Format(time.RFC3339),
		"age_seconds":            int64(360),
		"window_seconds":         int64(900),
		"warn_lead_seconds":      int64(600),
		"time_remaining_seconds": int64(540),
		"window_elapsed":         false,
	} {
		if r.Fields[k] != want {
			t.Errorf("Fields[%s] = %#v, want %#v", k, r.Fields[k], want)
		}
	}
	if !strings.Contains(r.Observed, "pending for 6m0s; decision window closes in 9m0s") || !strings.Contains(r.Observed, due.RunID.String()) {
		t.Errorf("Observed = %q", r.Observed)
	}
	if r.Threshold != "decided within 5m0s of request (15m0s window, 10m0s warning lead)" {
		t.Errorf("Threshold = %q", r.Threshold)
	}
	if len(reader.limits) != 1 || reader.limits[0] != amendmentScanLimit || reader.lookbacks[0] != 7*24*time.Hour {
		t.Errorf("scan args = lookbacks %v limits %v, want [168h] [%d]", reader.lookbacks, reader.limits, amendmentScanLimit)
	}
}

// TestAmendmentWindowProbe_ZeroWarnLeadFiresOnlyPastWindow (condition 1): a
// zero WarnLead is NO lead, never the default: a 6m row does not fire, a 16m
// row fires with window_elapsed.
func TestAmendmentWindowProbe_ZeroWarnLeadFiresOnlyPastWindow(t *testing.T) {
	young := amendRow(6*time.Minute, "running")
	got := windowProbe(&fakeAmendments{rows: []scopeamendment.PendingWithStage{young}}, 0).Check(context.Background())
	if len(got) != 1 || got[0].Status != StatusOK {
		t.Fatalf("6m row with a zero lead = %+v, want one ok result (no lead: not due before 15m)", got)
	}

	old := amendRow(16*time.Minute, "running")
	got = windowProbe(&fakeAmendments{rows: []scopeamendment.PendingWithStage{young, old}}, 0).Check(context.Background())
	if len(got) != 1 || got[0].Status != StatusFailing || got[0].Subject != old.ID.String() {
		t.Fatalf("results = %+v, want exactly one failing result for the 16m row", got)
	}
	if got[0].Fields["window_elapsed"] != true || got[0].Fields["warn_lead_seconds"] != int64(0) {
		t.Errorf("Fields = %v, want window_elapsed true and warn_lead_seconds 0", got[0].Fields)
	}
}

// TestAmendmentWindowProbe_WindowElapsedClampsRemaining: a 17m row reads
// time_remaining_seconds 0 and window_elapsed true.
func TestAmendmentWindowProbe_WindowElapsedClampsRemaining(t *testing.T) {
	row := amendRow(17*time.Minute, "dispatched")
	got := windowProbe(&fakeAmendments{rows: []scopeamendment.PendingWithStage{row}}, 10*time.Minute).Check(context.Background())
	if len(got) != 1 || got[0].Status != StatusFailing {
		t.Fatalf("results = %+v, want one failing result", got)
	}
	if got[0].Fields["time_remaining_seconds"] != int64(0) || got[0].Fields["window_elapsed"] != true {
		t.Errorf("Fields = %v, want time_remaining_seconds 0 and window_elapsed true", got[0].Fields)
	}
	if !strings.Contains(got[0].Observed, "decision window elapsed 2m0s ago") {
		t.Errorf("Observed = %q, want it to say the window elapsed 2m0s ago", got[0].Observed)
	}
}

// TestAmendmentWindowProbe_SkipsTerminalStageRows: an aged row on a
// succeeded stage is the terminal probe's, never a window alert.
func TestAmendmentWindowProbe_SkipsTerminalStageRows(t *testing.T) {
	row := amendRow(20*time.Minute, "succeeded")
	got := windowProbe(&fakeAmendments{rows: []scopeamendment.PendingWithStage{row}}, 10*time.Minute).Check(context.Background())
	if len(got) != 1 || got[0].Status != StatusOK {
		t.Fatalf("results = %+v, want one ok result (a terminal-stage row is not a window alert)", got)
	}
	if got[0].Detail["terminal"] != 1 || got[0].Detail["live"] != 0 {
		t.Errorf("Detail = %v, want terminal 1 live 0", got[0].Detail)
	}
}

// TestAmendmentWindowProbe_DefaultsForZeroWindowAndLookback: a zero Window
// and Lookback select the defaults (the WarnLead stays verbatim).
func TestAmendmentWindowProbe_DefaultsForZeroWindowAndLookback(t *testing.T) {
	reader := &fakeAmendments{rows: []scopeamendment.PendingWithStage{amendRow(14*time.Minute, "running")}}
	got := AmendmentWindowProbe{Amendments: reader}.Check(context.Background())
	if len(got) != 1 || got[0].Status != StatusOK {
		t.Fatalf("results = %+v, want ok: a 14m row is inside the default 15m window with no lead", got)
	}
	if reader.lookbacks[0] != DefaultAmendmentLookback {
		t.Errorf("lookback = %s, want the default %s", reader.lookbacks[0], DefaultAmendmentLookback)
	}
	if !strings.Contains(got[0].Observed, "1 pending amendment(s) scanned within 168h0m0s, none due") {
		t.Errorf("Observed = %q", got[0].Observed)
	}
}

// TestAmendmentWindowProbe_TruncatedScanStillSeesNewestDue (condition 2): a
// full 200-row page whose newest row is the only due live row yields that
// failing result PLUS an unknown naming the truncation; the truncation is
// never suppressed by the failing result.
func TestAmendmentWindowProbe_TruncatedScanStillSeesNewestDue(t *testing.T) {
	newest := amendRow(6*time.Minute, "running")
	rows := []scopeamendment.PendingWithStage{newest}
	for i := 1; i < amendmentScanLimit; i++ {
		rows = append(rows, amendRow(time.Duration(i)*time.Hour, "succeeded"))
	}
	got := windowProbe(&fakeAmendments{rows: rows}, 10*time.Minute).Check(context.Background())
	failing, unknowns := failingOf(got), unknownsOf(got)
	if len(got) != 2 || len(failing) != 1 || len(unknowns) != 1 {
		t.Fatalf("results = %+v, want one failing + one unknown", got)
	}
	if failing[0].Subject != newest.ID.String() {
		t.Errorf("failing subject = %s, want the newest row %s", failing[0].Subject, newest.ID)
	}
	if !strings.Contains(unknowns[0].Reason, "scan truncated at 200 rows") {
		t.Errorf("unknown reason = %q, want it to name the truncation", unknowns[0].Reason)
	}
}

// --- amendment_terminal -------------------------------------------------------

// TestAmendmentTerminalProbe_FiresOnTerminalStage: every terminal stage state
// fails amendment_pending_on_terminal_stage; a running-stage row does not.
func TestAmendmentTerminalProbe_FiresOnTerminalStage(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "cancelled", "superseded"} {
		t.Run(state, func(t *testing.T) {
			row, live := amendRow(3*time.Minute, state), amendRow(30*time.Minute, "running")
			got := AmendmentTerminalProbe{Amendments: &fakeAmendments{rows: []scopeamendment.PendingWithStage{row, live}}}.Check(context.Background())
			if len(got) != 1 || got[0].Status != StatusFailing {
				t.Fatalf("results = %+v, want exactly one failing result (the %s-stage row)", got, state)
			}
			r := got[0]
			if r.Probe != "amendment_terminal" || r.Condition != ConditionAmendmentPendingOnTerminalStage || r.Subject != row.ID.String() {
				t.Errorf("result = %+v", r)
			}
			if r.Fields["run_id"] != row.RunID.String() || r.Fields["stage_id"] != row.StageID.String() ||
				r.Fields["stage_state"] != state || r.Fields["age_seconds"] != int64(180) {
				t.Errorf("Fields = %v", r.Fields)
			}
			if !strings.Contains(r.Observed, "still pending; the stage is "+state) {
				t.Errorf("Observed = %q", r.Observed)
			}
		})
	}
}

// TestAmendmentTerminalProbe_LapsedRowExcluded: the #4127 interaction — a
// lapsed (and a denied) row on a succeeded stage is excluded by the
// classifier even when the reader returns it, so the probe reads ok.
func TestAmendmentTerminalProbe_LapsedRowExcluded(t *testing.T) {
	lapsed, denied := amendRow(time.Hour, "succeeded"), amendRow(time.Hour, "succeeded")
	lapsed.Status, denied.Status = scopeamendment.StatusLapsed, scopeamendment.StatusDenied
	got := AmendmentTerminalProbe{Amendments: &fakeAmendments{rows: []scopeamendment.PendingWithStage{lapsed, denied}}}.Check(context.Background())
	if len(got) != 1 || got[0].Status != StatusOK {
		t.Fatalf("results = %+v, want one ok result (non-pending rows never report)", got)
	}
	if got[0].Detail["not_pending"] != 2 || got[0].Detail["terminal"] != 0 {
		t.Errorf("Detail = %v, want not_pending 2 terminal 0", got[0].Detail)
	}
}

// --- shared failure modes -----------------------------------------------------

// TestAmendmentProbes_UnknownModes: one subtest per failure mode, for both
// probes. None reads ok.
func TestAmendmentProbes_UnknownModes(t *testing.T) {
	type mk func(AmendmentReader) Probe
	probes := map[string]struct {
		build mk
		due   func() scopeamendment.PendingWithStage
	}{
		"amendment_window": {
			build: func(r AmendmentReader) Probe { return windowProbe(r, 10*time.Minute) },
			due:   func() scopeamendment.PendingWithStage { return amendRow(8*time.Minute, "running") },
		},
		"amendment_terminal": {
			build: func(r AmendmentReader) Probe { return AmendmentTerminalProbe{Amendments: r} },
			due:   func() scopeamendment.PendingWithStage { return amendRow(time.Minute, "cancelled") },
		},
	}
	for name, p := range probes {
		t.Run(name+"/nil reader", func(t *testing.T) {
			got := p.build(nil).Check(context.Background())
			if len(got) != 1 || got[0].Status != StatusUnknown || got[0].Reason != "no database configured" {
				t.Errorf("results = %+v, want unknown (no database configured)", got)
			}
		})
		t.Run(name+"/reader error", func(t *testing.T) {
			got := p.build(&fakeAmendments{err: errors.New("relation exploded")}).Check(context.Background())
			if len(got) != 1 || got[0].Status != StatusUnknown || got[0].Reason != "scope-amendment scan failed: relation exploded" {
				t.Errorf("results = %+v, want unknown naming the scan failure", got)
			}
		})
		t.Run(name+"/hidden stage only", func(t *testing.T) {
			got := p.build(&fakeAmendments{rows: []scopeamendment.PendingWithStage{hiddenRow(time.Hour)}}).Check(context.Background())
			if len(got) != 1 || got[0].Status != StatusUnknown ||
				!strings.Contains(got[0].Reason, "1 pending amendment(s) whose stage is not visible to this session (row-level security)") {
				t.Errorf("results = %+v, want unknown naming the hidden count", got)
			}
		})
		t.Run(name+"/truncated scan with nothing due", func(t *testing.T) {
			rows := make([]scopeamendment.PendingWithStage, 0, amendmentScanLimit)
			for i := 0; i < amendmentScanLimit; i++ {
				rows = append(rows, amendRow(time.Second, "running"))
			}
			got := p.build(&fakeAmendments{rows: rows}).Check(context.Background())
			if len(got) != 1 || got[0].Status != StatusUnknown || !strings.Contains(got[0].Reason, "scan truncated at 200 rows") {
				t.Errorf("results = %+v, want unknown naming the truncation", got)
			}
		})
		t.Run(name+"/hidden alongside failing", func(t *testing.T) {
			due := p.due()
			got := p.build(&fakeAmendments{rows: []scopeamendment.PendingWithStage{hiddenRow(time.Hour), due}}).Check(context.Background())
			if len(got) != 1 || got[0].Status != StatusFailing || got[0].Subject != due.ID.String() {
				t.Errorf("results = %+v, want only the failing result", got)
			}
		})
	}
}

// TestAmendmentProbes_NotHostLocal: both probes run under a non-local
// deployment (they read the tracker, not the host).
func TestAmendmentProbes_NotHostLocal(t *testing.T) {
	reader := &fakeAmendments{}
	probes := []Probe{windowProbe(reader, 10*time.Minute), AmendmentTerminalProbe{Amendments: reader}}
	for _, p := range probes {
		if p.HostLocal() {
			t.Errorf("%s HostLocal() = true, want false", p.Name())
		}
	}
	got := Sweep(context.Background(), probes, DeploymentKubernetes, time.Minute)
	if len(got) != 2 {
		t.Fatalf("results = %+v, want two", got)
	}
	for _, r := range got {
		if r.Status != StatusOK {
			t.Errorf("%s = %+v, want ok under kubernetes (never not_applicable)", r.Probe, r)
		}
	}
	if len(reader.limits) != 2 {
		t.Errorf("reader called %d times, want 2 (both probes ran)", len(reader.limits))
	}
}

// TestAmendmentProbes_TickDedupPerRow: through the Ticker, each amendment id
// is its own (condition, subject): two Ticks inside the quiet window emit one
// row and one notification per id; past the window they re-emit.
func TestAmendmentProbes_TickDedupPerRow(t *testing.T) {
	termA, termB, live := amendRow(time.Hour, "succeeded"), amendRow(2*time.Hour, "failed"), amendRow(7*time.Minute, "running")
	reader := &fakeAmendments{rows: []scopeamendment.PendingWithStage{live, termA, termB}}
	au, n, h, clk := &fakeAudit{}, &countingNotifier{}, &captureHandler{}, newClock()
	tk := newTicker([]Probe{windowProbe(reader, 10*time.Minute), AmendmentTerminalProbe{Amendments: reader}}, au, n, clk, h)

	tk.Tick(context.Background())
	tk.Tick(context.Background())
	if au.appendCount() != 3 || n.count() != 3 {
		t.Fatalf("appends %d, notifies %d after two Ticks; want 3/3 (one per amendment id)", au.appendCount(), n.count())
	}
	seen := map[string]string{}
	for i := 0; i < 3; i++ {
		p := au.payload(t, i)
		seen[p["subject"].(string)] = p["condition"].(string)
	}
	want := map[string]string{
		live.ID.String():  ConditionAmendmentWindowClosing,
		termA.ID.String(): ConditionAmendmentPendingOnTerminalStage,
		termB.ID.String(): ConditionAmendmentPendingOnTerminalStage,
	}
	for id, cond := range want {
		if seen[id] != cond {
			t.Errorf("subject %s → %q, want %q (rows %v)", id, seen[id], cond, seen)
		}
	}

	clk.Advance(time.Hour + time.Minute)
	tk.Tick(context.Background())
	if au.appendCount() != 6 || n.count() != 6 {
		t.Errorf("appends %d, notifies %d after the quiet window; want 6/6 (each id re-emits)", au.appendCount(), n.count())
	}
}
