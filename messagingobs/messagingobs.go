package messagingobs

import (
	"context"
	"reflect"
	"time"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/LerianStudio/lib-observability/v4/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Metric names (OpenTelemetry messaging semantic conventions). Both are Float64
// seconds histograms, shared with lib-streaming so worker dashboards are
// transport-agnostic.
const (
	messagingClientOperationDurationMetric = "messaging.client.operation.duration"
	messagingProcessDurationMetric         = "messaging.process.duration"
)

// messagingSystemRabbitMQ is the messaging.system attribute value.
const messagingSystemRabbitMQ = "rabbitmq"

// messagingDurationBuckets follows the HTTP/RPC/Messaging advisory bucket layout
// from docs/metrics-contract.md.
var messagingDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.075,
	0.1, 0.25, 0.5, 0.75,
	1, 2.5, 5, 7.5, 10,
}

// newDurationHistogram builds a Float64 seconds histogram for the given metric
// name. Returns nil if the meter is nil or creation fails; callers treat nil as
// "do not record".
func newDurationHistogram(meter metric.Meter, name, description string) metric.Float64Histogram {
	if meter == nil {
		return nil
	}

	hist, err := meter.Float64Histogram(
		name,
		metric.WithUnit("s"),
		metric.WithDescription(description),
		metric.WithExplicitBucketBoundaries(messagingDurationBuckets...),
	)
	if err != nil {
		return nil
	}

	return hist
}

// telemetryEnabled reports whether the Telemetry has the components required to
// emit metrics, mirroring the middleware gate (MeterProvider AND MetricsFactory
// non-nil).
func telemetryEnabled(tl *tracing.Telemetry) bool {
	return tl != nil && tl.MeterProvider != nil && tl.MetricsFactory != nil
}

// tracerFor returns the library tracer when a TracerProvider is configured, or
// nil otherwise (callers create no span for nil).
func tracerFor(tl *tracing.Telemetry) trace.Tracer {
	if tl == nil || tl.TracerProvider == nil {
		return nil
	}

	return tl.TracerProvider.Tracer(tl.LibraryName)
}

// classifyErrorType returns a bounded error.type label for a failed messaging
// operation. It uses the Go error type name (a bounded set for a given service),
// never the error message, which could carry unbounded/PII content.
func classifyErrorType(err error) string {
	if err == nil {
		return ""
	}

	t := reflect.TypeOf(err)
	if t == nil {
		return "error"
	}

	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	name := t.String()
	if name == "" {
		return "error"
	}

	return constant.SanitizeMetricLabel(name)
}

// ProduceParams describes a single outbound message for instrumentation.
type ProduceParams struct {
	// DestinationTemplate is the LOW-CARDINALITY destination template, e.g.
	// "transactions.{tenant}". NEVER the concrete queue name or routing key.
	DestinationTemplate string
	// OperationName is the messaging.operation.name, e.g. "publish"/"send".
	OperationName string
	// RoutingKey and MessageID are accepted for the caller's own logging/span
	// body use; they are FORBIDDEN as labels and are NOT emitted as attributes.
	RoutingKey string
	MessageID  string
}

// ConsumeParams describes a single inbound message for instrumentation.
type ConsumeParams struct {
	// Headers are the inbound AMQP-style headers (Delivery.Headers) carrying the
	// propagated trace context.
	Headers map[string]any
	// DestinationTemplate is the LOW-CARDINALITY destination template.
	DestinationTemplate string
	// OperationName is the messaging.operation.name, e.g. "process"/"receive".
	OperationName string
	// ConsumerGroup is the messaging.consumer.group.name (bounded).
	ConsumerGroup string
	// RoutingKey and MessageID are accepted for the caller's own use; FORBIDDEN
	// as labels and NOT emitted as attributes.
	RoutingKey string
	MessageID  string
}

// FinishFunc records the operation duration and ends the span. Call it once,
// passing the operation's error (or nil on success). It is always safe to call,
// even for a no-op (nil-telemetry) helper.
type FinishFunc func(err error)

// Publisher instruments RabbitMQ produce operations. Build one with NewPublisher
// and reuse it; the duration histogram is created once at construction.
type Publisher struct {
	tel  *tracing.Telemetry
	hist metric.Float64Histogram
}

// NewPublisher returns a Publisher bound to the given Telemetry. The duration
// histogram is built once here (nil when telemetry is disabled), matching the
// instrument-once pattern used across the library.
func NewPublisher(tl *tracing.Telemetry) *Publisher {
	p := &Publisher{tel: tl}

	if telemetryEnabled(tl) {
		p.hist = newDurationHistogram(
			tl.MeterProvider.Meter(tl.LibraryName),
			messagingClientOperationDurationMetric,
			"Duration of messaging producer operations.",
		)
	}

	return p
}

// Produce starts a producer span, injects the trace context into a fresh header
// map for the caller to attach to the outgoing AMQP Publishing, and returns a
// FinishFunc that records messaging.client.operation.duration and ends the span.
//
// The returned headers are always non-nil so the caller can attach them
// unconditionally. With telemetry disabled the returned headers still carry any
// propagatable context and the FinishFunc is a safe no-op.
func (p *Publisher) Produce(ctx context.Context, params ProduceParams) (context.Context, map[string]any, FinishFunc) {
	if ctx == nil {
		ctx = context.Background()
	}

	var span trace.Span

	if tracer := tracerFor(p.tel); tracer != nil {
		ctx, span = tracer.Start(ctx, spanName(params.OperationName, params.DestinationTemplate),
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(baseMessagingAttrs(params.OperationName, params.DestinationTemplate)...),
		)
	}

	// Inject trace context into the headers the caller will publish. This works
	// from the (possibly span-updated) ctx regardless of whether a span was
	// created here.
	headers := make(map[string]any)
	tracing.InjectTraceHeadersIntoQueue(ctx, &headers)

	start := time.Now()

	finish := func(err error) {
		recordMessagingDuration(ctx, p.hist, params.OperationName, params.DestinationTemplate, "", start, err)
		finalizeSpan(span, err)
	}

	return ctx, headers, finish
}

// Consumer instruments RabbitMQ consume/process operations. Build one with
// NewConsumer and reuse it.
type Consumer struct {
	tel  *tracing.Telemetry
	hist metric.Float64Histogram
}

// NewConsumer returns a Consumer bound to the given Telemetry. The process
// duration histogram is built once here (nil when telemetry is disabled).
func NewConsumer(tl *tracing.Telemetry) *Consumer {
	c := &Consumer{tel: tl}

	if telemetryEnabled(tl) {
		c.hist = newDurationHistogram(
			tl.MeterProvider.Meter(tl.LibraryName),
			messagingProcessDurationMetric,
			"Duration of messaging consumer processing.",
		)
	}

	return c
}

// Consume extracts the trace context from the inbound headers (joining the
// producer's trace), starts a consumer span, and returns a FinishFunc that
// records messaging.process.duration and ends the span.
//
// With telemetry disabled the returned context is the extracted context (still
// useful for downstream propagation) and the FinishFunc is a safe no-op.
func (c *Consumer) Consume(ctx context.Context, params ConsumeParams) (context.Context, FinishFunc) {
	if ctx == nil {
		ctx = context.Background()
	}

	// Always extract inbound trace context so downstream work joins the trace,
	// even when this service's telemetry is disabled.
	ctx = tracing.ExtractTraceContextFromQueueHeaders(ctx, params.Headers)

	var span trace.Span

	if tracer := tracerFor(c.tel); tracer != nil {
		attrs := baseMessagingAttrs(params.OperationName, params.DestinationTemplate)
		if params.ConsumerGroup != "" {
			attrs = append(attrs, attribute.String("messaging.consumer.group.name",
				constant.SanitizeMetricLabel(params.ConsumerGroup)))
		}

		ctx, span = tracer.Start(ctx, spanName(params.OperationName, params.DestinationTemplate),
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(attrs...),
		)
	}

	start := time.Now()

	finish := func(err error) {
		recordMessagingDuration(ctx, c.hist, params.OperationName, params.DestinationTemplate,
			params.ConsumerGroup, start, err)
		finalizeSpan(span, err)
	}

	return ctx, finish
}

// baseMessagingAttrs builds the bounded attribute set shared by span and metric:
// messaging.system, messaging.operation.name, messaging.destination.template.
func baseMessagingAttrs(operationName, destinationTemplate string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 3)
	attrs = append(attrs, attribute.String("messaging.system", messagingSystemRabbitMQ))

	if operationName != "" {
		attrs = append(attrs, attribute.String("messaging.operation.name",
			constant.SanitizeMetricLabel(operationName)))
	}

	if destinationTemplate != "" {
		attrs = append(attrs, attribute.String("messaging.destination.template",
			constant.SanitizeMetricLabel(destinationTemplate)))
	}

	return attrs
}

// recordMessagingDuration emits a messaging duration observation. It is a no-op
// when the histogram is nil (telemetry disabled or creation failed). The label
// set carries ONLY bounded values; routing key and message id are never added.
func recordMessagingDuration(
	ctx context.Context,
	hist metric.Float64Histogram,
	operationName, destinationTemplate, consumerGroup string,
	start time.Time,
	err error,
) {
	if hist == nil {
		return
	}

	attrs := baseMessagingAttrs(operationName, destinationTemplate)

	if consumerGroup != "" {
		attrs = append(attrs, attribute.String("messaging.consumer.group.name",
			constant.SanitizeMetricLabel(consumerGroup)))
	}

	if errType := classifyErrorType(err); errType != "" {
		attrs = append(attrs, attribute.String("error.type", errType))
	}

	hist.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

// finalizeSpan records the error on the span (if any) and ends it. Safe on a nil
// span.
func finalizeSpan(span trace.Span, err error) {
	if span == nil {
		return
	}

	if err != nil {
		tracing.HandleSpanError(span, "messaging operation failed", err)
	}

	span.End()
}

// spanName builds an OTel-convention messaging span name "{operation} {template}"
// (both low-cardinality), falling back to whichever part is present.
func spanName(operationName, destinationTemplate string) string {
	switch {
	case operationName != "" && destinationTemplate != "":
		return operationName + " " + destinationTemplate
	case destinationTemplate != "":
		return destinationTemplate
	case operationName != "":
		return operationName
	default:
		return messagingSystemRabbitMQ
	}
}
