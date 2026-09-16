//go:build unit

package httpobs

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
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

// The duration METRIC labels and the span NAME are bounded and PII-free. The
// third surface, url.full on the span, is covered by
// TestNewTransport_SpanURLDropsQueryFragmentAndUserinfo: it keeps the path (an
// id in the path is still visible there) but never the query, fragment or
// userinfo, which is where credentials live.
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

// clientSpans returns the CLIENT spans recorded so far.
func clientSpans(t *testing.T, sr *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	t.Helper()

	var out []sdktrace.ReadOnlySpan

	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindClient {
			out = append(out, s)
		}
	}

	return out
}

// recordedRequest is what the test server actually received off the wire.
type recordedRequest struct {
	requestURI    string
	authorization string
	traceparent   string
}

// newRecordingServer serves 200 and records the request line and headers it got,
// so a test can assert the wire was left alone.
func newRecordingServer(t *testing.T, got *recordedRequest) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.requestURI = r.RequestURI
		got.authorization = r.Header.Get("Authorization")
		got.traceparent = r.Header.Get("Traceparent")

		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// A credential carried in the URL — Gemini's ?key=, a pre-signed S3/GCS
// signature, any ?token= — must never be exported to the collector. The span
// records scheme://host/path and nothing else; the request on the wire keeps the
// query, and the propagation header still rides on it.
func TestNewTransport_SpanURLDropsQueryFragmentAndUserinfo(t *testing.T) {
	mp, reader, tp, sr := newHarness(t)

	var got recordedRequest

	srv := newRecordingServer(t, &got)

	const (
		secret = "SECRET"
		query  = "key=SECRET&sig=S"
	)

	target, err := url.Parse(srv.URL + "/v1/x?" + query + "#frag")
	require.NoError(t, err)
	// http.Client turns userinfo into the Authorization header ABOVE the
	// transport (net/http client.go), so stripping it from the span's URL costs
	// the wire nothing.
	target.User = url.UserPassword("user", "pw")

	client := NewClient(nil,
		WithMeterProvider(mp),
		WithTracerProvider(tp),
		WithPropagators(propagation.TraceContext{}))
	doGET(t, client, target.String())

	spans := clientSpans(t, sr)
	require.Len(t, spans, 1)

	attrs := attribute.NewSet(spans[0].Attributes()...)

	recorded, ok := attrs.Value("url.full")
	require.True(t, ok, "the client span must still carry url.full")
	assert.Equal(t, srv.URL+"/v1/x", recorded.AsString(),
		"url.full must be scheme://host/path — no query, fragment or userinfo")

	for _, kv := range spans[0].Attributes() {
		v := kv.Value.Emit()
		assert.NotContains(t, v, secret, "the query credential leaked into span attribute %s", kv.Key)
		assert.NotContains(t, v, "sig=", "the query leaked into span attribute %s", kv.Key)
		assert.NotContains(t, v, "pw", "the userinfo leaked into span attribute %s", kv.Key)
	}

	for _, p := range collectClientDuration(t, reader) {
		for _, kv := range p.Attributes.ToSlice() {
			assert.NotContains(t, kv.Value.Emit(), secret, "the query credential leaked into metric label %s", kv.Key)
		}
	}

	// The wire is untouched: full query, the userinfo-derived Authorization, and
	// the trace context otelhttp injected below the scrubbing layer.
	assert.Equal(t, "/v1/x?"+query, got.requestURI, "the server must receive the full query")
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pw")), got.authorization,
		"the userinfo-derived Authorization must reach the server")
	assert.NotEmpty(t, got.traceparent, "trace context must still be propagated")
}

// A URL built with Opaque (the form net/url documents for request targets
// that must not be re-encoded) prints Opaque verbatim from String(), query
// included, and none of the other fields apply — so it is scrubbed too.
func TestNewTransport_SpanURLDropsOpaqueTarget(t *testing.T) {
	mp, _, tp, sr := newHarness(t)

	var got recordedRequest

	srv := newRecordingServer(t, &got)
	host := strings.TrimPrefix(srv.URL, "http://")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)
	req.URL = &url.URL{Scheme: "http", Host: host, Opaque: "//" + host + "/v1/x?key=SECRET"}

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	res, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, res.Body.Close())

	spans := clientSpans(t, sr)
	require.Len(t, spans, 1)

	for _, kv := range spans[0].Attributes() {
		assert.NotContains(t, kv.Value.Emit(), "SECRET", "the opaque target leaked into span attribute %s", kv.Key)
	}

	// RequestURI() prints an Opaque that starts with "//" as scheme + ":" + Opaque.
	assert.Equal(t, "http://"+host+"/v1/x?key=SECRET", got.requestURI,
		"the wire must still carry the opaque target whole")
}

// A URL with nothing to hide is recorded whole: the guarantee removes
// credentials, not the path.
func TestNewTransport_SpanURLKeepsSchemeHostAndPath(t *testing.T) {
	mp, _, tp, sr := newHarness(t)
	srv := newOKServer(t)

	client := NewClient(nil, WithMeterProvider(mp), WithTracerProvider(tp))
	doGET(t, client, srv.URL+"/v1/x")

	spans := clientSpans(t, sr)
	require.Len(t, spans, 1)

	attrs := attribute.NewSet(spans[0].Attributes()...)

	recorded, ok := attrs.Value("url.full")
	require.True(t, ok, "the client span must still carry url.full")
	assert.Equal(t, srv.URL+"/v1/x", recorded.AsString())
}

// A RoundTripper must not modify the request it was handed: http.Client reuses
// that request across redirects and retries, and one that lost its query there
// would silently drop the credential.
func TestNewTransport_LeavesTheCallersRequestAlone(t *testing.T) {
	_, _, tp, _ := newHarness(t)

	var got recordedRequest

	srv := newRecordingServer(t, &got)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/v1/x?key=SECRET", nil)
	require.NoError(t, err)

	resp, err := NewTransport(nil, WithTracerProvider(tp)).RoundTrip(req)
	require.NoError(t, err)

	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())

	assert.Equal(t, "key=SECRET", req.URL.RawQuery, "the caller's own request must come back untouched")
	assert.Equal(t, "/v1/x?key=SECRET", got.requestURI)
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
