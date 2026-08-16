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

func TestIsFailedRequestModel(t *testing.T) {
	// The predicate keys on the angle-bracket WRAPPER shape alone, so a real
	// model id — including a genuinely-unpriced freshly-released one — is
	// never misclassified as a failed request. That negative half is what
	// pins the no-false-positive property independently of the specific
	// "<synthetic>" literal Claude Code happens to emit today.
	cases := []struct {
		model string
		want  bool
	}{
		{"<synthetic>", true},
		{"<compaction>", true},
		{"  <synthetic>  ", true}, // surrounding whitespace is trimmed first
		{"<>", true},              // degenerate but still bracket-wrapped
		{"claude-fable-5", false}, // a genuinely-unpriced REAL model id
		{"claude-opus-4-8", false},
		{"gpt-5.6-terra", false},
		{"", false},
		{"<synthetic", false},   // unwrapped: opening bracket only
		{"synthetic>", false},   // unwrapped: closing bracket only
		{"a<synthetic>", false}, // bracket-bearing but not wrapped
		{"<synthetic>b", false},
		{"<", false}, // a single char cannot be both prefix and suffix
	}
	for _, tc := range cases {
		if got := IsFailedRequestModel(tc.model); got != tc.want {
			t.Errorf("IsFailedRequestModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestEvaluate_SyntheticModelIsFailedRequestNotUnpriced(t *testing.T) {
	// The behavioral heart of #2494: a placeholder model id lands in
	// FailedRequestModels and is EXCLUDED from UnpricedModels, while a real
	// unknown id yields the converse. The "<synthetic>" literal is seeded BY
	// CONSTRUCTION here (not produced by calling the classifier), so deleting
	// the routing branch in Evaluate reddens the UnpricedModels assertion
	// rather than the fixture.
	t.Run("placeholder", func(t *testing.T) {
		samples := []Sample{
			{Time: base.Add(-1 * time.Hour), Model: "<synthetic>", KnownModel: false, KnownUsage: true},
		}

		d := Evaluate(samples, nil, base, Window)

		if !reflect.DeepEqual(d.FailedRequestModels, []string{"<synthetic>"}) {
			t.Errorf("FailedRequestModels = %v, want [<synthetic>]", d.FailedRequestModels)
		}
		if len(d.UnpricedModels) != 0 {
			t.Errorf("UnpricedModels = %v, want empty — a failed request is not an unpriced model", d.UnpricedModels)
		}
		if len(d.UnknownUsageModels) != 0 {
			t.Errorf("UnknownUsageModels = %v, want empty", d.UnknownUsageModels)
		}
		if !d.FailedRequestTripped {
			t.Error("FailedRequestTripped = false, want true")
		}
		if d.Tripped {
			t.Error("Tripped = true, want false — no unpriced/unknown-usage model in this window")
		}
	})

	t.Run("real_unknown_model", func(t *testing.T) {
		samples := []Sample{
			{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
		}

		d := Evaluate(samples, nil, base, Window)

		if !reflect.DeepEqual(d.UnpricedModels, []string{"claude-fable-5"}) {
			t.Errorf("UnpricedModels = %v, want [claude-fable-5]", d.UnpricedModels)
		}
		if len(d.FailedRequestModels) != 0 {
			t.Errorf("FailedRequestModels = %v, want empty", d.FailedRequestModels)
		}
		if !d.Tripped || d.FailedRequestTripped {
			t.Errorf("Tripped/FailedRequestTripped = %v/%v, want true/false", d.Tripped, d.FailedRequestTripped)
		}
	})
}

func TestEvaluate_BothClassesTripIndependently(t *testing.T) {
	// One window carrying both a placeholder row and a real unpriced row trips
	// both gates, with disjoint model sets — the two alerts emit independently.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "<synthetic>", KnownModel: false, KnownUsage: false},
		{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
	}

	d := Evaluate(samples, nil, base, Window)

	if !d.Tripped || !d.FailedRequestTripped {
		t.Fatalf("Tripped/FailedRequestTripped = %v/%v, want true/true", d.Tripped, d.FailedRequestTripped)
	}
	if !reflect.DeepEqual(d.UnpricedModels, []string{"claude-fable-5"}) {
		t.Errorf("UnpricedModels = %v, want [claude-fable-5]", d.UnpricedModels)
	}
	if !reflect.DeepEqual(d.FailedRequestModels, []string{"<synthetic>"}) {
		t.Errorf("FailedRequestModels = %v, want [<synthetic>]", d.FailedRequestModels)
	}
	// known_usage=false on the placeholder must NOT leak into the secondary
	// unpriced set either — the routing excludes it from BOTH.
	if len(d.UnknownUsageModels) != 0 {
		t.Errorf("UnknownUsageModels = %v, want empty", d.UnknownUsageModels)
	}
}

func TestEvaluate_FailedRequestEvidenceFromSummedTokens(t *testing.T) {
	// The evidence sums the four token buckets across the reported
	// failed-request samples and derives cache_read / (fresh + read + write).
	samples := []Sample{
		{Time: base.Add(-2 * time.Hour), Model: "<synthetic>",
			InputTokens: 10, CacheReadInputTokens: 60, CacheWriteInputTokens: 10, OutputTokens: 1},
		{Time: base.Add(-1 * time.Hour), Model: "<synthetic>",
			InputTokens: 10, CacheReadInputTokens: 60, CacheWriteInputTokens: 10, OutputTokens: 3},
	}

	d := Evaluate(samples, nil, base, Window)

	ev := d.FailedRequestEvidence
	if ev.InputTokens != 20 || ev.CacheReadInputTokens != 120 ||
		ev.CacheWriteInputTokens != 20 || ev.OutputTokens != 4 {
		t.Fatalf("evidence = %+v, want summed 20/120/20/4", ev)
	}
	// 120 / (20 + 120 + 20) = 0.75
	if ev.CacheReadRatio != 0.75 {
		t.Errorf("CacheReadRatio = %v, want 0.75", ev.CacheReadRatio)
	}
}

func TestEvaluate_FailedRequestZeroDenominatorRatioIsZero(t *testing.T) {
	// Failure mode (d): a manifest that reported NO usage at all
	// (known_usage=false, every token bucket 0) still classifies as a failed
	// request, and the ratio is 0 — never a NaN from a zero denominator.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "<synthetic>", KnownModel: false, KnownUsage: false},
	}

	d := Evaluate(samples, nil, base, Window)

	if !d.FailedRequestTripped {
		t.Fatalf("expected a failed-request trip, got %+v", d)
	}
	if got := d.FailedRequestEvidence.CacheReadRatio; got != 0 {
		t.Errorf("CacheReadRatio = %v, want exactly 0 (zero denominator)", got)
	}
}

func TestEvaluate_ClassesDoNotCrossSuppress(t *testing.T) {
	// The one-time transition decision, option (a) — STRICT INDEPENDENCE
	// (#2494 approval condition 2). This is the exact case the reviewer
	// named: a prior-window unpriced_model_alert whose model list contains
	// "<synthetic>" (how historical occurrences were recorded), then a new
	// window containing "<synthetic>". The prior UNPRICED-class alert must
	// NOT suppress the first failed-request alert, so the operator sees one
	// duplicate report at the single upgrade boundary rather than silence.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "<synthetic>", KnownModel: false, KnownUsage: true},
	}
	priorUnpriced := []Alert{
		{Time: base.Add(-30 * time.Minute), Model: "<synthetic>", FailedRequest: false},
	}

	d := Evaluate(samples, priorUnpriced, base, Window)

	if !d.FailedRequestTripped {
		t.Fatalf("a prior UNPRICED-class alert must not suppress the failed-request class: %+v", d)
	}
	if !reflect.DeepEqual(d.FailedRequestModels, []string{"<synthetic>"}) {
		t.Errorf("FailedRequestModels = %v, want [<synthetic>]", d.FailedRequestModels)
	}

	// The reciprocal: a prior FAILED-REQUEST-class alert does not suppress a
	// same-id unpriced classification either. A real model id is used here
	// because a bracketed id can never reach the unpriced set at all.
	unpricedSamples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "claude-fable-5", KnownModel: false, KnownUsage: true},
	}
	priorFailed := []Alert{
		{Time: base.Add(-30 * time.Minute), Model: "claude-fable-5", FailedRequest: true},
	}
	d2 := Evaluate(unpricedSamples, priorFailed, base, Window)
	if !d2.Tripped {
		t.Fatalf("a prior FAILED-REQUEST-class alert must not suppress the unpriced class: %+v", d2)
	}
}

func TestEvaluate_FailedRequestDedupsAgainstOwnStream(t *testing.T) {
	// Within its own class the once-per-window dedup still applies: a prior
	// in-window agent_request_failed_alert naming the id suppresses a re-alarm,
	// and the suppressed sample contributes nothing to the evidence.
	samples := []Sample{
		{Time: base.Add(-1 * time.Hour), Model: "<synthetic>", CacheReadInputTokens: 500},
	}
	priorAlerts := []Alert{
		{Time: base.Add(-30 * time.Minute), Model: "<synthetic>", FailedRequest: true},
	}

	d := Evaluate(samples, priorAlerts, base, Window)

	if d.FailedRequestTripped {
		t.Fatalf("expected suppression by the prior same-class alert, got %+v", d)
	}
	if d.FailedRequestEvidence.CacheReadInputTokens != 0 {
		t.Errorf("evidence counted a suppressed sample: %+v", d.FailedRequestEvidence)
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
