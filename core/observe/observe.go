// Package observe projects the append-only HX event log into OpenTelemetry
// spans. The log remains the sole source of truth; export is read-only and
// best-effort.
package observe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Eastsidegunn/JANUS/contracts/gen"
	"github.com/Eastsidegunn/JANUS/core/logd"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// AdapterVersions is an immutable-by-convention catalog produced from the
// adapter manifest/fixture metadata. A missing version is an error; it is
// never replaced with "unknown".
type AdapterVersions map[string]string

// SpanData is the deterministic, receiver-independent OTel projection.
type SpanData struct {
	Name       string
	Context    trace.SpanContext
	Parent     trace.SpanContext
	Start      time.Time
	End        time.Time
	Attributes []attribute.KeyValue
}

type aggregate struct {
	traceID, spanID, parent string
	start, end              int64
	spawn                   *gen.SubagentSpawnPayload
	done                    *gen.SubagentDonePayload
}

// Project validates the whole snapshot before returning any span. IDs are
// decoded without regeneration, padding, or transformation.
func Project(events []gen.EventRecord, versions AdapterVersions) ([]SpanData, error) {
	state, err := logd.Replay(events)
	if err != nil {
		return nil, err
	}
	aggregates := map[string]*aggregate{}
	for _, event := range events {
		a := aggregates[event.SpanID]
		if a == nil {
			a = &aggregate{traceID: event.TraceID, spanID: event.SpanID, start: event.Ts, end: event.Ts}
			if event.ParentSpanID != nil {
				a.parent = *event.ParentSpanID
			}
			aggregates[event.SpanID] = a
		}
		if event.Ts < a.start {
			a.start = event.Ts
		}
		if event.Ts > a.end {
			a.end = event.Ts
		}
		switch event.Kind {
		case gen.KindSubagentSpawn:
			var payload gen.SubagentSpawnPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("observe: spawn seq %d: %w", event.Seq, err)
			}
			a.spawn = &payload
		case gen.KindSubagentDone:
			var payload gen.SubagentDonePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("observe: done seq %d: %w", event.Seq, err)
			}
			a.done = &payload
		}
	}

	keys := make([]string, 0, len(aggregates))
	for key := range aggregates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	spans := make([]SpanData, 0, len(keys))
	for _, key := range keys {
		a := aggregates[key]
		traceID, err := trace.TraceIDFromHex(a.traceID)
		if err != nil {
			return nil, fmt.Errorf("observe: trace_id: %w", err)
		}
		spanID, err := trace.SpanIDFromHex(a.spanID)
		if err != nil {
			return nil, fmt.Errorf("observe: span_id: %w", err)
		}
		context := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
		parent := trace.SpanContext{}
		if a.parent != "" {
			parentID, err := trace.SpanIDFromHex(a.parent)
			if err != nil {
				return nil, fmt.Errorf("observe: parent_span_id: %w", err)
			}
			parent = trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: parentID})
		}
		name := "hx.session"
		attrs := []attribute.KeyValue{}
		if a.spawn != nil {
			if a.spawn.ProfileID == nil || *a.spawn.ProfileID == "" {
				return nil, fmt.Errorf("observe: span %s profile_id 누락", a.spanID)
			}
			version := versions[a.spawn.Adapter]
			if version == "" {
				return nil, fmt.Errorf("observe: adapter %q version 누락", a.spawn.Adapter)
			}
			if a.done == nil {
				return nil, fmt.Errorf("observe: span %s 종료 상태 누락", a.spanID)
			}
			usage := state.UsageBySpan[a.spanID]
			name = "hx.subagent"
			attrs = []attribute.KeyValue{
				attribute.String("hx.adapter.name", a.spawn.Adapter),
				attribute.String("hx.adapter.version", version),
				attribute.String("hx.profile.id", *a.spawn.ProfileID),
				attribute.String("hx.status", string(a.done.Status)),
				attribute.Int64("hx.usage.input_tokens", usage.In),
				attribute.Int64("hx.usage.output_tokens", usage.Out),
			}
		}
		sort.Slice(attrs, func(i, j int) bool { return string(attrs[i].Key) < string(attrs[j].Key) })
		spans = append(spans, SpanData{Name: name, Context: context, Parent: parent, Start: time.UnixMilli(a.start), End: time.UnixMilli(a.end), Attributes: attrs})
	}
	return spans, nil
}

// NewOTLPHTTPExporter is the only production exporter constructor. The exact
// module version is pinned in go.mod; gRPC and auto-instrumentation are not
// configured here.
func NewOTLPHTTPExporter(ctx context.Context, options ...otlptracehttp.Option) (sdktrace.SpanExporter, error) {
	return otlptracehttp.New(ctx, options...)
}

type fixedIDGenerator struct {
	current trace.SpanContext
}

func (g *fixedIDGenerator) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	return g.current.TraceID(), g.current.SpanID()
}

func (g *fixedIDGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	return g.current.SpanID()
}

type exportProcessor struct {
	ctx      context.Context
	exporter sdktrace.SpanExporter
	err      error
	suppress bool
}

func (*exportProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (p *exportProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if p.suppress {
		return
	}
	p.err = errors.Join(p.err, p.exporter.ExportSpans(p.ctx, []sdktrace.ReadOnlySpan{span}))
}

func (*exportProcessor) Shutdown(context.Context) error   { return nil }
func (*exportProcessor) ForceFlush(context.Context) error { return nil }

// ExportBestEffort deliberately returns no error to the session path. Export
// failures are delivered only to report; nil exporter is the default noop.
func ExportBestEffort(ctx context.Context, exporter sdktrace.SpanExporter, spans []SpanData, report func(error)) {
	if exporter == nil {
		return
	}
	generator := &fixedIDGenerator{}
	processor := &exportProcessor{ctx: ctx, exporter: exporter}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(generator),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(resource.Empty()),
		sdktrace.WithSpanProcessor(processor),
	)
	tracer := provider.Tracer("github.com/Eastsidegunn/JANUS/core/observe")
	for _, data := range spans {
		generator.current = data.Context
		parent := ctx
		if data.Parent.IsValid() {
			parent = trace.ContextWithSpanContext(parent, data.Parent)
		}
		_, span := tracer.Start(parent, data.Name,
			trace.WithTimestamp(data.Start),
			trace.WithAttributes(data.Attributes...),
		)
		if got := span.SpanContext(); got.TraceID() != data.Context.TraceID() || got.SpanID() != data.Context.SpanID() {
			processor.suppress = true
			span.End(trace.WithTimestamp(data.End))
			processor.suppress = false
			processor.err = errors.Join(processor.err, fmt.Errorf("observe: SDK가 로그 ID를 보존하지 않음"))
			break
		}
		span.End(trace.WithTimestamp(data.End))
	}
	_ = provider.Shutdown(context.WithoutCancel(ctx))
	if processor.err != nil && report != nil {
		report(processor.err)
	}
}
