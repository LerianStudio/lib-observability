//go:build unit

package httpobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const httpClientRequestDurationMetric = "http.client.request.duration"

func newHarness(t *testing.T) (*sdkmetric.MeterProvider, *sdkmetric.ManualReader, *sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	return mp, reader, tp, sr
}

// doGET drives one instrumented outbound request against a local test server and
// fully reads/closes the body so the span ends.
func doGET(t *testing.T, client *http.Client, url string) {
	t.Helper()
	resp, err := client.Get(url)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
}

func collectClientDuration(t *testing.T, reader *sdkmetric.ManualReader) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var points []metricdata.HistogramDataPoint[float64]
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != httpClientRequestDurationMetric {
				continue
			}
			require.Equal(t, "s", m.Unit, "http client duration must be seconds")
			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected float64 histogram, got %T", m.Data)
			points = append(points, h.DataPoints...)
		}
	}
	return points
}

func assertHasKey(t *testing.T, set attribute.Set, key string) {
	t.Helper()
	_, ok := set.Value(attribute.Key(key))
	assert.True(t, ok, "expected attribute %q to be present", key)
}

func assertNoKey(t *testing.T, set attribute.Set, key string) {
	t.Helper()
	_, ok := set.Value(attribute.Key(key))
	assert.False(t, ok, "attribute %q must NOT be present (PII/cardinality guardrail)", key)
}

func newOKServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewTransport_EmitsClientDurationMetric(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)
	srv := newOKServer(t)

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	doGET(t, client, srv.URL)

	points := collectClientDuration(t, reader)
	require.NotEmpty(t, points, "expected http.client.request.duration to be emitted")

	set := points[0].Attributes
	assertHasKey(t, set, "http.request.method")
	assertHasKey(t, set, "server.address")
	assertNoKey(t, set, "url.path")
	assertNoKey(t, set, "url.query")
}

func TestNewTransport_ProducesClientSpan(t *testing.T) {
	mp, _, tp, sr := newHarness(t)
	srv := newOKServer(t)

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	doGET(t, client, srv.URL)

	var clientSpans int
	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			clientSpans++
		}
	}
	assert.GreaterOrEqual(t, clientSpans, 1, "outbound request must produce a CLIENT span")
}

// httpobs guarantees the two surfaces it OWNS are PII/cardinality-safe: the
// duration METRIC labels and the span NAME. The raw request URL (url.full) is a
// standard semconv attribute that otelhttp always sets on the client span and
// that OTel-Go offers no supported hook to remove; PII redaction of url.full is
// therefore delegated to the OTel Collector (transform processor) — see ADR-008.
func TestNewTransport_MetricLabelsAndSpanNameAreBounded(t *testing.T) {
	mp, reader, tp, sr := newHarness(t)
	srv := newOKServer(t)

	const secretPath = "/users/123.456.789-00"
	const secretQuery = "pix_key=123.456.789-00"

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	doGET(t, client, srv.URL+secretPath+"?"+secretQuery)

	// Metric labels must NEVER carry the path/query/PII (cardinality + cost).
	points := collectClientDuration(t, reader)
	require.NotEmpty(t, points)
	for _, p := range points {
		for _, kv := range p.Attributes.ToSlice() {
			v := kv.Value.AsString()
			assert.NotContains(t, v, "123.456.789-00", "PII leaked into metric label %s", kv.Key)
			assert.NotContains(t, v, secretPath, "url.path leaked into metric label %s", kv.Key)
		}
		assertNoKey(t, p.Attributes, "url.path")
		assertNoKey(t, p.Attributes, "url.query")
		assertNoKey(t, p.Attributes, "url.full")
	}

	// The span NAME must be bounded — never the concrete path/PII.
	for _, s := range sr.Ended() {
		assert.NotContains(t, s.Name(), "123.456.789-00", "PII leaked into span name")
		assert.NotContains(t, s.Name(), secretPath, "url.path leaked into span name")
	}
}

func TestNewTransport_NilBaseUsesDefaultTransport(t *testing.T) {
	rt := NewTransport(nil)
	require.NotNil(t, rt)
}

func TestNewClient_NoProvidersDoesNotPanic(t *testing.T) {
	srv := newOKServer(t)

	client := NewClient(nil) // no meter/tracer providers → no-op, must not break the call
	require.NotPanics(t, func() { doGET(t, client, srv.URL) })
}

func TestNewTransport_NoTracerProviderProducesNoSpan(t *testing.T) {
	mp, _, _, sr := newHarness(t)
	srv := newOKServer(t)

	client := NewClient(nil, WithMeterProvider(mp)) // metric only, no tracer (ADR-005)
	doGET(t, client, srv.URL)

	for _, s := range sr.Ended() {
		assert.NotEqual(t, trace.SpanKindClient, s.SpanKind(),
			"no CLIENT span expected without a TracerProvider")
	}
}
