package otelemit

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// attrMap flattens a span's attributes into a lookup keyed by string.
func attrMap(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	m := make(map[attribute.Key]attribute.Value, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

func TestEmitStage_AttributesAndTraceID(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	e := New(exp)

	const runID = "11111111-2222-3333-4444-555555555555"
	e.EmitStage(context.Background(), StageSpan{
		RunID:        runID,
		Stage:        "implement",
		Model:        "claude-opus-4-8",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
		Latency:      3 * time.Second,
		OK:           true,
	})
	// ForceFlush (not Shutdown) before reading: tracetest's
	// InMemoryExporter clears its store on Shutdown.
	if err := e.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (stage + model-call)", len(spans))
	}

	// Deterministic trace id: both spans share TraceIDFromRunID(runID).
	wantTrace := TraceIDFromRunID(runID)
	for _, s := range spans {
		if s.SpanContext.TraceID() != wantTrace {
			t.Errorf("span %q trace id = %s, want %s", s.Name, s.SpanContext.TraceID(), wantTrace)
		}
	}

	// Locate the model-call span and assert the GenAI + cost attrs.
	var call *tracetest.SpanStub
	for i := range spans {
		if spans[i].Name == "chat claude-opus-4-8" {
			call = &spans[i]
		}
	}
	if call == nil {
		t.Fatalf("model-call span not found; names = %v", spanNames(spans))
	}

	// The stage span records OK as an Ok status.
	for i := range spans {
		if spans[i].Name == "stage implement" && spans[i].Status.Code != codes.Ok {
			t.Errorf("stage span status = %v, want Ok", spans[i].Status.Code)
		}
	}
	a := attrMap(call.Attributes)
	if got := a["gen_ai.request.model"].AsString(); got != "claude-opus-4-8" {
		t.Errorf("gen_ai.request.model = %q", got)
	}
	if got := a["gen_ai.usage.input_tokens"].AsInt64(); got != 1_000_000 {
		t.Errorf("input_tokens = %d", got)
	}
	if got := a["gen_ai.usage.output_tokens"].AsInt64(); got != 1_000_000 {
		t.Errorf("output_tokens = %d", got)
	}
	// 1M input @ $5/1M + 1M output @ $25/1M = $30.
	if got := a["fishhawk.cost.usd"].AsFloat64(); got < 29.999 || got > 30.001 {
		t.Errorf("fishhawk.cost.usd = %v, want ~30", got)
	}
	if !a["fishhawk.cost.priced"].AsBool() {
		t.Error("fishhawk.cost.priced = false, want true for known model")
	}
	// Temperature is unavailable from claude stream-json (G6 best-effort).
	if a["fishhawk.repro.temperature_available"].AsBool() {
		t.Error("temperature_available = true, want false (claude does not expose it)")
	}
}

func TestEmitStage_UnknownModelPricedZero(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	e := New(exp)
	e.EmitStage(context.Background(), StageSpan{
		RunID:        "run-x",
		Stage:        "plan",
		Model:        "some-future-model",
		InputTokens:  100,
		OutputTokens: 100,
	})
	_ = e.ForceFlush(context.Background())

	for _, s := range exp.GetSpans() {
		if s.Name != "chat some-future-model" {
			continue
		}
		a := attrMap(s.Attributes)
		if a["fishhawk.cost.priced"].AsBool() {
			t.Error("priced = true for unknown model, want false")
		}
		if got := a["fishhawk.cost.usd"].AsFloat64(); got != 0 {
			t.Errorf("cost.usd = %v for unknown model, want 0", got)
		}
	}
}

func TestDisabledEmitter_NoOp(t *testing.T) {
	// Bootstrap with the gate env unset → disabled, methods no-op.
	t.Setenv(EndpointEnv, "")
	e, err := Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if e.Enabled() {
		t.Fatal("Enabled() = true with endpoint unset, want false")
	}
	// Must not panic with no provider.
	e.EmitStage(context.Background(), StageSpan{RunID: "r", Stage: "plan"})
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on disabled emitter: %v", err)
	}
}

func TestTraceIDFromRunID_DeterministicAndValid(t *testing.T) {
	a := TraceIDFromRunID("run-abc")
	b := TraceIDFromRunID("run-abc")
	if a != b {
		t.Error("trace id not deterministic for same run id")
	}
	if !a.IsValid() {
		t.Error("derived trace id is not valid (zero)")
	}
	if TraceIDFromRunID("run-abc") == TraceIDFromRunID("run-xyz") {
		t.Error("different run ids produced identical trace ids")
	}
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i := range spans {
		out[i] = spans[i].Name
	}
	return out
}

var _ sdktrace.SpanExporter = (*tracetest.InMemoryExporter)(nil)
