// Package otelemit emits OpenTelemetry GenAI-semantic-convention
// spans for a single runner stage invocation: a stage span with a
// child model-call span carrying token counts, latency, and an
// estimated cost.
//
// The runner spawns a fresh short-lived process per
// fishhawk_run_stage, so a run-level trace cannot share an
// in-process parent across stages. We stitch the per-stage processes
// into one trace by deriving a deterministic 16-byte trace id from
// the RunID (TraceIDFromRunID): every stage of the same run emits
// spans under the same trace id, parented to a synthesized run-root
// span context. See the IDGenerator note in the plan
// (https://pkg.go.dev/go.opentelemetry.io/otel/sdk/trace#IDGenerator);
// we achieve the same stitching with a synthesized parent
// SpanContext rather than a custom generator, which keeps the
// model-call span's own id random.
//
// Emission is gated by OTEL_EXPORTER_OTLP_ENDPOINT: when unset,
// Bootstrap returns a disabled Emitter whose methods are no-ops, so
// the implement loop is completely unaffected. When set, an
// OTLP/HTTP exporter is configured (honouring the standard
// OTEL_EXPORTER_OTLP_* env vars) behind a batch processor that the
// caller MUST force-flush via Shutdown before the process exits, or
// buffered spans are lost.
package otelemit

import (
	"context"
	"crypto/sha256"
	"os"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/kuhlman-labs/fishhawk/pricing"
)

// EndpointEnv is the standard OTel env var that gates emission. When
// it is empty, Bootstrap returns a disabled Emitter.
const EndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// serviceName is the resource service.name stamped on every span.
const serviceName = "fishhawk-runner"

// Emitter owns the TracerProvider for a runner process. A nil tracer
// (the disabled state) makes every method a no-op.
type Emitter struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
}

// StageSpan is the data captured for one stage invocation. It maps
// onto a stage span plus a child model-call span.
type StageSpan struct {
	RunID        string
	Stage        string
	Model        string
	InputTokens  int
	OutputTokens int
	Latency      time.Duration

	// OK records whether the stage succeeded; emitted as a span
	// status so a failed stage is distinguishable in the trace.
	OK bool

	// Temperature is the resolved sampling temperature when the agent
	// surfaced one. Claude Code's stream-json does NOT expose it
	// today, so this is best-effort (G6): nil records temperature as
	// unavailable rather than guessing.
	Temperature *float64
}

// Bootstrap constructs an Emitter. When OTEL_EXPORTER_OTLP_ENDPOINT
// is unset it returns a disabled Emitter (all methods no-op) and a
// nil error, so callers wire it unconditionally. When set it
// configures an OTLP/HTTP exporter behind a batch processor.
func Bootstrap(ctx context.Context) (*Emitter, error) {
	if os.Getenv(EndpointEnv) == "" {
		return &Emitter{}, nil
	}
	// otlptracehttp.New reads the standard OTEL_EXPORTER_OTLP_*
	// environment (endpoint, headers, TLS) so operators configure the
	// collector entirely through env, matching every other OTel SDK.
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	return New(exp), nil
}

// New builds an enabled Emitter around an explicit span exporter.
// This is the seam tests use to substitute an in-memory exporter;
// production goes through Bootstrap.
func New(exp sdktrace.SpanExporter) *Emitter {
	res, _ := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(serviceName),
	))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		// AlwaysSample: the synthesized run-root parent is marked
		// sampled, and we want every gated emission exported.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	return &Emitter{tp: tp, tracer: tp.Tracer("github.com/kuhlman-labs/fishhawk/runner/internal/otelemit")}
}

// Enabled reports whether spans are actually emitted.
func (e *Emitter) Enabled() bool { return e != nil && e.tracer != nil }

// EmitStage records one stage span with a child model-call span. A
// disabled Emitter returns immediately. The spans share the
// deterministic run-level trace id (TraceIDFromRunID) so all of a
// run's stages stitch into one trace across the separate runner
// processes.
func (e *Emitter) EmitStage(ctx context.Context, s StageSpan) {
	if !e.Enabled() {
		return
	}

	// Synthesize a run-root parent span context so the stage span
	// inherits the deterministic trace id. The parent is marked
	// sampled and remote=false; the SDK reuses its trace id for the
	// child and generates a fresh span id.
	parent := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    TraceIDFromRunID(s.RunID),
		SpanID:     runRootSpanID(s.RunID),
		TraceFlags: trace.FlagsSampled,
	})
	ctx = trace.ContextWithSpanContext(ctx, parent)

	start := time.Now()
	if s.Latency > 0 {
		// Anchor the span timestamps to the observed latency window so
		// the span duration reflects the model call, not wall-clock
		// time spent building spans.
		start = start.Add(-s.Latency)
	}

	stageCtx, stageSpan := e.tracer.Start(ctx, "stage "+s.Stage,
		trace.WithTimestamp(start),
		trace.WithAttributes(
			attribute.String("fishhawk.run_id", s.RunID),
			attribute.String("fishhawk.stage", s.Stage),
		),
	)

	cost, priced := pricing.Cost(s.Model, s.InputTokens, s.OutputTokens)
	callAttrs := []attribute.KeyValue{
		// GenAI semantic conventions:
		// https://opentelemetry.io/docs/specs/semconv/gen-ai/
		attribute.String("gen_ai.system", "anthropic"),
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", s.Model),
		attribute.Int("gen_ai.usage.input_tokens", s.InputTokens),
		attribute.Int("gen_ai.usage.output_tokens", s.OutputTokens),
		// Cost + reproducibility (G6). Cost is always an estimate from
		// a dated table; priced=false (unknown model) records 0.
		attribute.Float64("fishhawk.cost.usd", cost),
		attribute.Bool("fishhawk.cost.estimated", true),
		attribute.Bool("fishhawk.cost.priced", priced),
		attribute.String("fishhawk.pricing.as_of", pricing.AsOf),
		attribute.Int64("fishhawk.latency_ms", s.Latency.Milliseconds()),
	}
	if s.Temperature != nil {
		callAttrs = append(callAttrs,
			attribute.Float64("gen_ai.request.temperature", *s.Temperature),
			attribute.Bool("fishhawk.repro.temperature_available", true),
		)
	} else {
		// Record the absence explicitly so a future claude version
		// that starts emitting temperature is a visible signal change,
		// not a silent gap.
		callAttrs = append(callAttrs,
			attribute.Bool("fishhawk.repro.temperature_available", false),
		)
	}

	_, callSpan := e.tracer.Start(stageCtx, "chat "+s.Model,
		trace.WithTimestamp(start),
		trace.WithAttributes(callAttrs...),
	)
	// Record the stage outcome as a span status so a failed stage is
	// distinguishable from a clean one in the trace.
	if s.OK {
		stageSpan.SetStatus(codes.Ok, "")
	} else {
		stageSpan.SetStatus(codes.Error, "stage failed")
	}

	end := start.Add(s.Latency)
	callSpan.End(trace.WithTimestamp(end))
	stageSpan.End(trace.WithTimestamp(end))
}

// ForceFlush exports any spans buffered by the batch processor
// without tearing the provider down. A disabled Emitter returns nil.
func (e *Emitter) ForceFlush(ctx context.Context) error {
	if e == nil || e.tp == nil {
		return nil
	}
	return e.tp.ForceFlush(ctx)
}

// Shutdown force-flushes buffered spans and tears down the provider.
// It MUST be called before the short-lived runner process exits or
// batched spans are dropped
// (https://pkg.go.dev/go.opentelemetry.io/otel/sdk/trace#TracerProvider.Shutdown).
// A disabled Emitter returns nil.
func (e *Emitter) Shutdown(ctx context.Context) error {
	if e == nil || e.tp == nil {
		return nil
	}
	return e.tp.Shutdown(ctx)
}

// TraceIDFromRunID derives a deterministic, valid 16-byte trace id
// from a run id. The same run id always yields the same trace id, so
// every per-stage runner process of one run emits under a single
// trace. A sha256 prefix is non-zero for any input, so the result is
// always a valid (non-zero) TraceID.
func TraceIDFromRunID(runID string) trace.TraceID {
	sum := sha256.Sum256([]byte(runID))
	var tid trace.TraceID
	copy(tid[:], sum[:16])
	return tid
}

// runRootSpanID derives a deterministic span id for the synthesized
// run-root parent. Distinct domain separator from the trace id so it
// never collides with a hypothetical span id derived the same way.
func runRootSpanID(runID string) trace.SpanID {
	sum := sha256.Sum256([]byte("fishhawk-run-root:" + runID))
	var sid trace.SpanID
	copy(sid[:], sum[:8])
	return sid
}
