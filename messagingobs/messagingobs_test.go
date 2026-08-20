//go:build unit

package messagingobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LerianStudio/lib-observability/v3/metrics"
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newHarness(t *testing.T) (*tracing.Telemetry, *sdkmetric.ManualReader, *tracetest.InMemoryExporter) {
	t.Helper()

	// Set the global text-map propagator exactly as the library bootstrap does
	// (tracing.otel.go). The queue trace helpers rely on
	// otel.GetTextMapPropagator(); without this it is the no-op propagator and no
	// headers are injected — the same behavior the app would see if it never
	// called InitializeGlobalTelemetry.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	factory, err := metrics.NewMetricsFactory(mp.Meter("test-library"), nil)
	require.NoError(t, err)

	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	tel := &tracing.Telemetry{
		TelemetryConfig: tracing.TelemetryConfig{
			LibraryName:     "test-library",
			EnableTelemetry: true,
		},
		TracerProvider: tp,
		MeterProvider:  mp,
		MetricsFactory: factory,
	}

	return tel, reader, spanExp
}

func findHistogram(t *testing.T, reader *sdkmetric.ManualReader, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var points []metricdata.HistogramDataPoint[float64]

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}

			require.Equal(t, "s", m.Unit, "%s must be seconds", name)

			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected float64 histogram for %s, got %T", name, m.Data)
			points = append(points, h.DataPoints...)
		}
	}

	return points
}

func attrString(set attribute.Set, key string) (string, bool) {
	v, ok := set.Value(attribute.Key(key))
	if !ok {
		return "", false
	}

	return v.AsString(), true
}

// TestProduce_RecordsDurationInjectsTraceAndOmitsForbidden verifies the producer
// helper: records messaging.client.operation.duration (seconds) with the
// low-cardinality labels, injects trace context into the returned AMQP headers,
// and never emits routing key / message id as a label.
func TestProduce_RecordsDurationInjectsTraceAndOmitsForbidden(t *testing.T) {
	tel, reader, spanExp := newHarness(t)

	pub := NewPublisher(tel)

	ctx, headers, finish := pub.Produce(context.Background(), ProduceParams{
		DestinationTemplate: "transactions.{tenant}",
		OperationName:       "publish",
		// These MUST NOT become labels:
		RoutingKey: "transactions.acme.pix.9f3c",
		MessageID:  "0f8fad5b-d9cb-469f-a165-70867728950e",
	})
	require.NotNil(t, ctx)

	// Trace context must be injected into the headers the caller will attach to
	// the AMQP publishing. The lib's queue helpers canonicalize the header key
	// via textproto (e.g. "Traceparent"), so match case-insensitively.
	require.NotEmpty(t, headers, "producer must inject trace headers")
	assert.True(t, hasTraceparent(headers), "traceparent must be injected for propagation, got %v", headers)

	finish(nil)

	points := findHistogram(t, reader, messagingClientOperationDurationMetric)
	require.Len(t, points, 1, "exactly one produce observation expected")

	dp := points[0]

	system, ok := attrString(dp.Attributes, "messaging.system")
	require.True(t, ok)
	assert.Equal(t, "rabbitmq", system)

	op, ok := attrString(dp.Attributes, "messaging.operation.name")
	require.True(t, ok)
	assert.Equal(t, "publish", op)

	dest, ok := attrString(dp.Attributes, "messaging.destination.template")
	require.True(t, ok)
	assert.Equal(t, "transactions.{tenant}", dest)

	// FORBIDDEN labels (docs/metrics-contract.md): routing key & message id.
	assertNoForbiddenMessagingLabels(t, dp.Attributes)

	// The span must also exist and be free of forbidden attributes.
	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)

	for _, kv := range spans[0].Attributes {
		val := kv.Value.Emit()
		assert.NotContains(t, val, "transactions.acme.pix.9f3c", "span leaked routing key: %s", kv.Key)
		assert.NotContains(t, val, "0f8fad5b", "span leaked message id: %s", kv.Key)
	}
}

// TestConsume_ExtractsTraceRecordsProcessDuration verifies the consumer helper:
// extracts trace context from inbound headers (joining the producer's trace),
// records messaging.process.duration (seconds), and carries the consumer group.
func TestConsume_ExtractsTraceRecordsProcessDuration(t *testing.T) {
	tel, reader, spanExp := newHarness(t)

	pub := NewPublisher(tel)

	// Produce first to obtain headers carrying a trace context.
	_, headers, finishProduce := pub.Produce(context.Background(), ProduceParams{
		DestinationTemplate: "transactions.{tenant}",
		OperationName:       "publish",
	})
	finishProduce(nil)

	require.True(t, hasTraceparent(headers), "produce must inject traceparent, got %v", headers)

	con := NewConsumer(tel)

	ctx, finish := con.Consume(context.Background(), ConsumeParams{
		Headers:             headers,
		DestinationTemplate: "transactions.{tenant}",
		OperationName:       "process",
		ConsumerGroup:       "ledger-workers",
		RoutingKey:          "transactions.acme.pix.9f3c",
		MessageID:           "0f8fad5b-d9cb-469f-a165-70867728950e",
	})
	require.NotNil(t, ctx)

	finish(nil)

	points := findHistogram(t, reader, messagingProcessDurationMetric)
	require.Len(t, points, 1)

	dp := points[0]

	system, ok := attrString(dp.Attributes, "messaging.system")
	require.True(t, ok)
	assert.Equal(t, "rabbitmq", system)

	op, ok := attrString(dp.Attributes, "messaging.operation.name")
	require.True(t, ok)
	assert.Equal(t, "process", op)

	group, ok := attrString(dp.Attributes, "messaging.consumer.group.name")
	require.True(t, ok)
	assert.Equal(t, "ledger-workers", group)

	assertNoForbiddenMessagingLabels(t, dp.Attributes)

	// The consumer span must be linked to the producer's trace (same trace id).
	spans := spanExp.GetSpans()
	require.NotEmpty(t, spans)

	var producerTraceID, consumerTraceID string
	for _, s := range spans {
		if s.SpanKind.String() == "producer" {
			producerTraceID = s.SpanContext.TraceID().String()
		}

		if s.SpanKind.String() == "consumer" {
			consumerTraceID = s.SpanContext.TraceID().String()
		}
	}

	require.NotEmpty(t, producerTraceID)
	require.NotEmpty(t, consumerTraceID)
	assert.Equal(t, producerTraceID, consumerTraceID,
		"consumer span must join the producer's trace via header propagation")
}

// TestProduce_ErrorSetsErrorTypeLabel verifies a failed produce records the
// bounded error.type label.
func TestProduce_ErrorSetsErrorTypeLabel(t *testing.T) {
	tel, reader, _ := newHarness(t)

	pub := NewPublisher(tel)

	_, _, finish := pub.Produce(context.Background(), ProduceParams{
		DestinationTemplate: "transactions.{tenant}",
		OperationName:       "publish",
	})

	finish(errors.New("broker unreachable"))

	points := findHistogram(t, reader, messagingClientOperationDurationMetric)
	require.Len(t, points, 1)

	errType, ok := attrString(points[0].Attributes, "error.type")
	require.True(t, ok, "error.type must be set on a failed produce")
	assert.NotEmpty(t, errType)
}

// TestProduce_NoProvidersIsNoOp verifies the producer degrades to a safe no-op
// against the (uninstalled) globals: it still returns usable trace headers and
// never panics.
func TestProduce_NoProvidersIsNoOp(t *testing.T) {
	pub := NewPublisherWithOptions()

	ctx, headers, finish := pub.Produce(context.Background(), ProduceParams{
		DestinationTemplate: "x",
		OperationName:       "publish",
	})
	require.NotNil(t, ctx)
	require.NotNil(t, headers)

	assert.NotPanics(t, func() { finish(nil) })
}

// TestConsume_NoProvidersIsNoOp verifies the consumer degrades to a safe no-op.
func TestConsume_NoProvidersIsNoOp(t *testing.T) {
	con := NewConsumerWithOptions()

	ctx, finish := con.Consume(context.Background(), ConsumeParams{
		Headers:             map[string]any{},
		DestinationTemplate: "x",
		OperationName:       "process",
	})
	require.NotNil(t, ctx)

	assert.NotPanics(t, func() { finish(nil) })
}

// hasTraceparent reports whether the AMQP-style header map carries a W3C
// traceparent under any case variant of the key.
func hasTraceparent(headers map[string]any) bool {
	for k := range headers {
		if strings.EqualFold(k, "traceparent") {
			return true
		}
	}

	return false
}

// assertNoForbiddenMessagingLabels asserts none of the FORBIDDEN messaging
// labels (routing key, message id, and their common attribute keys) appear on
// the metric.
func assertNoForbiddenMessagingLabels(t *testing.T, set attribute.Set) {
	t.Helper()

	forbiddenKeys := []string{
		"messaging.rabbitmq.destination.routing_key",
		"messaging.message.id",
		"routing_key",
		"message_id",
		"messaging.destination.name", // concrete queue/routing name (unbounded)
	}

	for _, k := range forbiddenKeys {
		_, ok := set.Value(attribute.Key(k))
		assert.False(t, ok, "forbidden messaging label %q must not be present", k)
	}

	// Also verify no attribute VALUE contains the concrete routing key / id.
	for _, kv := range set.ToSlice() {
		val := kv.Value.Emit()
		assert.NotContains(t, val, "transactions.acme.pix.9f3c",
			"metric label %q leaked routing key: %s", kv.Key, val)
		assert.NotContains(t, val, "0f8fad5b",
			"metric label %q leaked message id: %s", kv.Key, val)
	}
}

// installGlobals points the OTel globals at SDK providers wired to the returned
// reader/exporter, exactly as Telemetry.ApplyGlobals does at service bootstrap,
// and restores the previous globals afterwards.
func installGlobals(t *testing.T) (*sdkmetric.ManualReader, *tracetest.InMemoryExporter) {
	t.Helper()

	prevMP, prevTP := otel.GetMeterProvider(), otel.GetTracerProvider()

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spanExp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(spanExp))

	otel.SetMeterProvider(mp)
	otel.SetTracerProvider(tp)

	t.Cleanup(func() {
		otel.SetMeterProvider(prevMP)
		otel.SetTracerProvider(prevTP)
		_ = mp.Shutdown(context.Background())
		_ = tp.Shutdown(context.Background())
	})

	return reader, spanExp
}

// TestProduce_ZeroOptionsUsesGlobalProviders is the contract that lets a library
// instrument on the service's behalf: with NO options at all the publisher binds
// to the globals the service installed, so the service emits producer telemetry
// on a dependency bump alone, without passing anything down.
func TestProduce_ZeroOptionsUsesGlobalProviders(t *testing.T) {
	reader, spanExp := installGlobals(t)

	pub := NewPublisherWithOptions()

	_, headers, finish := pub.Produce(context.Background(), ProduceParams{
		DestinationTemplate: "transactions.{tenant}",
		OperationName:       "publish",
	})
	finish(nil)

	assert.True(t, hasTraceparent(headers), "trace context must be injected into the headers")

	points := findHistogram(t, reader, messagingClientOperationDurationMetric)
	require.Len(t, points, 1, "the global MeterProvider must receive the duration")

	system, ok := attrString(points[0].Attributes, "messaging.system")
	require.True(t, ok)
	assert.Equal(t, "rabbitmq", system)

	spans := spanExp.GetSpans()
	require.Len(t, spans, 1, "the global TracerProvider must receive the PRODUCER span")
	assert.Equal(t, defaultLibraryName, spans[0].InstrumentationScope.Name)
}

// TestConsume_ZeroOptionsUsesGlobalProviders is the consumer half of the same
// contract.
func TestConsume_ZeroOptionsUsesGlobalProviders(t *testing.T) {
	reader, spanExp := installGlobals(t)

	con := NewConsumerWithOptions()

	_, finish := con.Consume(context.Background(), ConsumeParams{
		Headers:             map[string]any{},
		DestinationTemplate: "transactions.{tenant}",
		OperationName:       "process",
	})
	finish(nil)

	require.Len(t, findHistogram(t, reader, messagingProcessDurationMetric), 1)
	require.Len(t, spanExp.GetSpans(), 1)
}

// TestOptions_ExplicitProvidersWinOverGlobals verifies an explicitly injected
// provider is used instead of the installed global one.
func TestOptions_ExplicitProvidersWinOverGlobals(t *testing.T) {
	globalReader, _ := installGlobals(t)

	ownReader := sdkmetric.NewManualReader()
	ownMP := sdkmetric.NewMeterProvider(sdkmetric.WithReader(ownReader))

	t.Cleanup(func() { _ = ownMP.Shutdown(context.Background()) })

	pub := NewPublisherWithOptions(WithMeterProvider(ownMP), WithLibraryName("explicit-scope"))

	_, _, finish := pub.Produce(context.Background(), ProduceParams{
		DestinationTemplate: "x",
		OperationName:       "publish",
	})
	finish(nil)

	assert.Len(t, findHistogram(t, ownReader, messagingClientOperationDurationMetric), 1,
		"the injected MeterProvider must receive the duration")
	assert.Empty(t, findHistogram(t, globalReader, messagingClientOperationDurationMetric),
		"the global MeterProvider must not be used when one was injected")
}

// TestOptions_NilAndEmptyValuesAreIgnored verifies the options never downgrade
// the resolved config: a nil provider or empty name leaves the default in place,
// as does a nil Telemetry.
func TestOptions_NilAndEmptyValuesAreIgnored(t *testing.T) {
	cfg := newConfig(
		WithMeterProvider(nil),
		WithTracerProvider(nil),
		WithLibraryName(""),
		WithTelemetry(nil),
		WithTelemetry(&tracing.Telemetry{}),
	)

	assert.Equal(t, otel.GetMeterProvider(), cfg.meterProvider)
	assert.Equal(t, otel.GetTracerProvider(), cfg.tracerProvider)
	assert.Equal(t, defaultLibraryName, cfg.libraryName)
}

// TestNilReceiversAreSafe verifies a nil helper never panics, so a caller
// holding an unbuilt publisher/consumer degrades instead of crashing the
// publish path.
func TestNilReceiversAreSafe(t *testing.T) {
	var (
		pub *Publisher
		con *Consumer
	)

	assert.NotPanics(t, func() {
		_, headers, finish := pub.Produce(nil, ProduceParams{OperationName: "publish"}) //nolint:staticcheck // nil ctx is the case under test
		require.NotNil(t, headers)
		finish(errors.New("boom"))
	})

	assert.NotPanics(t, func() {
		_, finish := con.Consume(nil, ConsumeParams{OperationName: "process"}) //nolint:staticcheck // nil ctx is the case under test
		finish(nil)
	})
}
