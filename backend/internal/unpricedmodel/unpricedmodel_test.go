package unpricedmodel

import (
	"reflect"
	"testing"
	"time"
)

// base is a fixed UTC instant so the window math lands on a predictable
// boundary and the tests don't depend on wall-clock timing.
var base = time.Date(2026, 6, 2, 12, 30, 0, 0, time.UTC)

func TestEvaluate_TripsOnUnpricedModel(t *testing.T) {
	// A single in-window cost row for an unpriced model (KnownModel=false)
	// trips and names that model in UnpricedModels.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
	}

	d := Evaluate(samples, nil, base, Window)

	if !d.Tripped {
		t.Fatalf("expected trip on known_model=false, got %+v", d)
	}
	if !reflect.DeepEqual(d.UnpricedModels, []string{"claude-fable-5"}) {
		t.Errorf("UnpricedModels = %v, want [claude-fable-5]", d.UnpricedModels)
	}
	if len(d.UnknownUsageModels) != 0 {
		t.Errorf("UnknownUsageModels = %v, want empty", d.UnknownUsageModels)
	}
}

func TestEvaluate_TripsOnUnknownUsage(t *testing.T) {
	// A priced model that reported no usage (KnownUsage=false) trips via
	// the secondary UnknownUsageModels set while UnpricedModels stays empty.
	samples := []Sample{
		{Time: base.Add(-30 * time.Minute), Model: "claude-opus-4-8", KnownModel: true, KnownUsage: false},
	}

	d := Evaluate(samples, nil, base, Window)

	if !d.Tripped {
		t.Fatalf("expected trip on known_usage=false, got %+v", d)
	}
	if len(d.UnpricedModels) != 0 {
		t.Errorf("UnpricedModels = %v, want empty", d.UnpricedModels)
	}
	if !reflect.DeepEqual(d.UnknownUsageModels, []string{"claude-opus-4-8"}) {
		t.Errorf("UnknownUsageModels = %v, want [claude-opus-4-8]", d.UnknownUsageModels)
	}
}

func TestEvaluate_QuietWhenAllKnown(t *testing.T) {
	// Every in-window row is both priced and reported usage — no trip.
	samples := []Sample{
		{Time: base.Add(-2 * time.Hour), Model: "claude-opus-4-8", KnownModel: true, KnownUsage: true},
		{Time: base.Add(-1 * time.Hour), Model: "claude-sonnet-5", KnownModel: true, KnownUsage: true},
	}

	d := Evaluate(samples, nil, base, Window)

	if d.Tripped {
		t.Fatalf("did not expect trip when all models are known: %+v", d)
	}
	if d.UnpricedModels != nil || d.UnknownUsageModels != nil {
		t.Errorf("expected nil model sets, got unpriced=%v unknownUsage=%v",
			d.UnpricedModels, d.UnknownUsageModels)
	}
}

func TestEvaluate_ExcludesSamplesOlderThanWindow(t *testing.T) {
	// An unpriced row just outside the window is ignored; only the
	// in-window all-known row remains, so nothing trips.
	samples := []Sample{
		{Time: base.Add(-Window - time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
		{Time: base.Add(-1 * time.Hour), Model: "claude-opus-4-8", KnownModel: true, KnownUsage: true},
	}

	d := Evaluate(samples, nil, base, Window)

	if d.Tripped {
		t.Fatalf("did not expect trip — the unpriced row is out of window: %+v", d)
	}
}

func TestEvaluate_DedupsModelWithPriorInWindowAlert(t *testing.T) {
	// One unpriced model already has a prior in-window alert and is
	// suppressed; a distinct still-unalarmed unpriced model still trips.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
		{Time: base.Add(-1 * time.Hour), Model: "gpt-5.6-terra", KnownModel: false, KnownUsage: true},
	}
	priorAlerts := []Alert{
		{Time: base.Add(-30 * time.Minute), Model: "claude-fable-5"},
	}

	d := Evaluate(samples, priorAlerts, base, Window)

	if !d.Tripped {
		t.Fatalf("expected trip for the still-unalarmed model, got %+v", d)
	}
	if !reflect.DeepEqual(d.UnpricedModels, []string{"gpt-5.6-terra"}) {
		t.Errorf("UnpricedModels = %v, want [gpt-5.6-terra] (claude-fable-5 deduped)", d.UnpricedModels)
	}
}

func TestEvaluate_PriorAlertOlderThanWindowDoesNotSuppress(t *testing.T) {
	// A prior alert older than the window no longer suppresses the model —
	// the once-per-window dedup re-alarms after the horizon passes.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
	}
	priorAlerts := []Alert{
		{Time: base.Add(-Window - time.Hour), Model: "claude-fable-5"},
	}

	d := Evaluate(samples, priorAlerts, base, Window)

	if !d.Tripped {
		t.Fatalf("expected trip — the prior alert is out of window: %+v", d)
	}
	if !reflect.DeepEqual(d.UnpricedModels, []string{"claude-fable-5"}) {
		t.Errorf("UnpricedModels = %v, want [claude-fable-5]", d.UnpricedModels)
	}
}

func TestEvaluate_CollapsesRepeatedSameModelRows(t *testing.T) {
	// Many unpriced rows for the same model collapse to one entry — the
	// payload names the offending model once, not once per row.
	samples := []Sample{
		{Time: base.Add(-3 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
		{Time: base.Add(-2 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
		{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
	}

	d := Evaluate(samples, nil, base, Window)

	if !reflect.DeepEqual(d.UnpricedModels, []string{"claude-fable-5"}) {
		t.Errorf("UnpricedModels = %v, want a single [claude-fable-5]", d.UnpricedModels)
	}
}

func TestEvaluate_DefaultsWindowWhenNonPositive(t *testing.T) {
	// A non-positive window falls back to the 24h Window const, and the
	// Decision reports the applied window + its start.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
	}

	d := Evaluate(samples, nil, base, 0)

	if d.Window != Window {
		t.Errorf("Window = %v, want default %v", d.Window, Window)
	}
	if want := base.Add(-Window); !d.WindowStart.Equal(want) {
		t.Errorf("WindowStart = %v, want %v", d.WindowStart, want)
	}
	if !d.Tripped {
		t.Error("expected trip under the defaulted window")
	}
}

func TestEvaluate_SkipsEmptyModelIDs(t *testing.T) {
	// A cost row with an empty model id carries no offender to name, so it
	// never trips the alert even when its flags are false.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "", KnownModel: false, KnownUsage: false},
	}

	d := Evaluate(samples, nil, base, Window)

	if d.Tripped {
		t.Fatalf("did not expect trip for an empty model id: %+v", d)
	}
}
