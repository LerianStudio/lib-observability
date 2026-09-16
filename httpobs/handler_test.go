//go:build unit

package httpobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const httpServerRequestDurationMetric = "http.server.request.duration"

// remoteParent is a well-formed W3C traceparent with the sampled flag set.
const (
	remoteParent  = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	remoteTraceID = "0af7651916cd43dd8448eb211c80319c"
	remoteSpanID  = "b7ad6b7169203331"
)

// serveOne drives exactly one inbound request through h and returns the served
// path. The handler is mounted on a real server so the request travels the wire,
// headers and all.
func serveOne(t *testing.T, h http.Handler, path string, header http.Header) {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	require.NoError(t, err)

	for k, vs := range header {
		if k == "Host" {
			req.Host = vs[0]

			continue
		}

		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// muxOn registers pattern on a Go 1.22+ ServeMux so the request carries
// r.Pattern by the time the wrapper names the span.
func muxOn(pattern string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

func serverSpans(t *testing.T, sr *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	t.Helper()

	var out []sdktrace.ReadOnlySpan

	for _, s := range sr.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			out = append(out, s)
		}
	}

	return out
}

func TestNewHandler_ProducesServerSpanNamedFromPattern(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	h := NewHandler(muxOn("/users/{id}"), WithTracerProvider(tp))
	serveOne(t, h, "/users/123.456.789-00", nil)

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1, "one inbound request must produce exactly one SERVER span")

	span := spans[0]
	assert.Equal(t, "GET /users/{id}", span.Name(), "span name must be method + route pattern")
	assert.NotContains(t, span.Name(), "123.456.789-00", "the concrete path must never reach the span name")

	var methodSeen bool

	for _, kv := range span.Attributes() {
		if string(kv.Key) == "http.request.method" {
			methodSeen = true

			assert.Equal(t, http.MethodGet, kv.Value.AsString())
		}
	}

	assert.True(t, methodSeen, "server span must carry http.request.method")
}

// A ServeMux pattern is "[METHOD ][HOST]/[PATH]", so a method-qualified
// registration already carries the method. Every registration style must produce
// the same span name, with the method appearing exactly once.
func TestNewHandler_DefaultSpanNameNeverRepeatsTheMethod(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
		path    string
		host    string
		want    string
	}{
		{"method qualified", "GET /users/{id}", "/users/42", "", "GET /users/{id}"},
		{"tab separated", "GET\t/users/{id}", "/users/42", "", "GET /users/{id}"},
		{"run of separators", "GET \t /users/{id}", "/users/42", "", "GET /users/{id}"},
		{"unqualified", "/users/{id}", "/users/42", "", "GET /users/{id}"},
		{"method and host qualified", "GET example.com/x", "/x", "example.com", "GET example.com/x"},
		{"host qualified only", "example.com/y", "/y", "example.com", "GET example.com/y"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, tp, sr := newHarness(t)

			var header http.Header
			if tc.host != "" {
				header = http.Header{"Host": []string{tc.host}}
			}

			h := NewHandler(muxOn(tc.pattern), WithTracerProvider(tp))
			serveOne(t, h, tc.path, header)

			spans := serverSpans(t, sr)
			require.Len(t, spans, 1)
			assert.Equal(t, tc.want, spans[0].Name())
			assert.Equal(t, 1, strings.Count(spans[0].Name(), http.MethodGet),
				"the method must appear exactly once in %q", spans[0].Name())
		})
	}
}

func TestNewHandler_UnmatchedRequestFallsBackToMethodOnly(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	// A bare handler (no ServeMux) never sets r.Pattern.
	h := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), WithTracerProvider(tp))
	serveOne(t, h, "/anything/123.456.789-00", nil)

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)
	assert.Equal(t, "GET", spans[0].Name(), "with no route pattern the name is the method alone")
}

func TestNewHandler_IgnoresInboundTraceContextByDefault(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	h := NewHandler(muxOn("/users/{id}"), WithTracerProvider(tp))
	serveOne(t, h, "/users/1", http.Header{"Traceparent": []string{remoteParent}})

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	span := spans[0]
	assert.False(t, span.Parent().IsValid(), "an untrusted caller must not become the parent")
	assert.NotEqual(t, remoteTraceID, span.SpanContext().TraceID().String(),
		"an untrusted caller must not choose this service's trace id")
}

func TestNewHandler_WithPropagatorsContinuesTheCallersTrace(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	h := NewHandler(muxOn("/users/{id}"),
		WithTracerProvider(tp),
		WithPropagators(propagation.TraceContext{}))
	serveOne(t, h, "/users/1", http.Header{"Traceparent": []string{remoteParent}})

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	span := spans[0]
	require.True(t, span.Parent().IsValid(), "an explicit propagator must join the inbound trace")
	assert.Equal(t, remoteSpanID, span.Parent().SpanID().String())
	assert.Equal(t, remoteTraceID, span.SpanContext().TraceID().String())
}

func TestNewHandler_HonoursSpanNameFormatter(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	h := NewHandler(muxOn("/users/{id}"),
		WithTracerProvider(tp),
		WithSpanNameFormatter(func(_ string, r *http.Request) string { return "inbound " + r.Method }))
	serveOne(t, h, "/users/1", nil)

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)
	assert.Equal(t, "inbound GET", spans[0].Name())
}

func TestNewHandler_EmitsServerDurationMetric(t *testing.T) {
	mp, reader, tp, _ := newHarness(t)

	h := NewHandler(muxOn("/users/{id}"), WithTracerProvider(tp), WithMeterProvider(mp))
	serveOne(t, h, "/users/1", nil)

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var found bool

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != httpServerRequestDurationMetric {
				continue
			}

			found = true

			assert.Equal(t, "s", m.Unit, "http server duration must be seconds")

			h, ok := m.Data.(metricdata.Histogram[float64])
			require.True(t, ok, "expected float64 histogram, got %T", m.Data)
			require.NotEmpty(t, h.DataPoints)
			assertHasKey(t, h.DataPoints[0].Attributes, "http.request.method")
			assertNoKey(t, h.DataPoints[0].Attributes, "url.query")
		}
	}

	assert.True(t, found, "expected %s to be emitted", httpServerRequestDurationMetric)
}

func TestNewHandler_NoProvidersStillServesAndProducesNoSpan(t *testing.T) {
	_, _, _, sr := newHarness(t) // recorder attached to a provider that is NOT global

	var served bool

	h := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served = true

		w.WriteHeader(http.StatusOK)
	}))

	require.NotPanics(t, func() { serveOne(t, h, "/", nil) })
	assert.True(t, served, "the wrapped handler must still be served with no providers")
	assert.Empty(t, serverSpans(t, sr), "no SERVER span without a TracerProvider")
}

// The Authorization header and the request/response bodies must never reach the
// span. otelhttp does not record them; this pins that the wrapper adds nothing.
func TestNewHandler_NeverRecordsSecretsOrBodies(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	const secret = "Bearer super-secret-token"

	h := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("response-body-marker"))
	}), WithTracerProvider(tp))
	serveOne(t, h, "/", http.Header{"Authorization": []string{secret}})

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	for _, kv := range spans[0].Attributes() {
		v := kv.Value.Emit()
		assert.NotContains(t, v, "super-secret-token", "Authorization leaked into attribute %s", kv.Key)
		assert.NotContains(t, v, "response-body-marker", "response body leaked into attribute %s", kv.Key)
	}

	for _, ev := range spans[0].Events() {
		for _, kv := range ev.Attributes {
			assert.NotContains(t, kv.Value.Emit(), "super-secret-token")
		}
	}
}
