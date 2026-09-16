//go:build unit

package httpobs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

// An OAuth callback route receives ?code=…&state=…. otelhttp's SERVER semconv
// records url.path and NEVER the query or a full URL (internal/semconv/server.go:
// "attrs = append(attrs, semconv.URLPath(req.URL.Path))"), so nothing has to be
// scrubbed on the inbound side. This pins that, and that the handler below still
// reads the query it was sent.
func TestNewHandler_ServerSpanNeverRecordsTheQueryString(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	const secret = "SECRET"

	var gotCode string

	// Registered on a mux, so url.path is the route template — which for a
	// static route is the path itself, query still excluded.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/callback", func(w http.ResponseWriter, r *http.Request) {
		gotCode = r.URL.Query().Get("code")

		w.WriteHeader(http.StatusOK)
	})

	h := NewHandler(mux, WithTracerProvider(tp))
	serveOne(t, h, "/v1/callback?code="+secret+"&state=xyz", nil)

	assert.Equal(t, secret, gotCode, "the handler must still receive the full query")

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	var pathSeen bool

	for _, kv := range spans[0].Attributes() {
		assert.NotContains(t, kv.Value.Emit(), secret, "the query credential leaked into attribute %s", kv.Key)

		if string(kv.Key) == "url.path" {
			pathSeen = true

			assert.Equal(t, "/v1/callback", kv.Value.AsString())
		}
	}

	assert.True(t, pathSeen, "server span must carry url.path")
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

// spanAttrSet collects one span's attributes so the assertHasKey/assertNoKey
// helpers can be used on them.
func spanAttrSet(span sdktrace.ReadOnlySpan) attribute.Set {
	return attribute.NewSet(span.Attributes()...)
}

// url.path on the SERVER span is the route TEMPLATE, never the concrete path:
// an id, a CPF or an account number sitting in the path must not leave the
// process. otelhttp records the concrete path at span start; the wrapper
// overwrites it once the mux has resolved the route.
// otelhttp ends the span in its own defer, so a handler that panics would
// export the concrete path recorded at span start unless the rewrite is
// deferred too.
func TestNewHandler_PanickingHandlerStillRecordsTheRouteTemplate(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users/{id}", func(http.ResponseWriter, *http.Request) { panic("boom") })

	h := NewHandler(mux, WithTracerProvider(tp))
	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)

	assert.PanicsWithValue(t, "boom", func() { h.ServeHTTP(httptest.NewRecorder(), req) })

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	attrs := spanAttrSet(spans[0])
	path, ok := attrs.Value("url.path")
	require.True(t, ok)
	assert.Equal(t, "/users/{id}", path.AsString(), "the panic must not leak the concrete path")
}

// A host-qualified pattern names the span with its host, but url.path and
// http.route are path templates and start at the slash.
func TestNewHandler_HostQualifiedPatternRecordsOnlyThePathTemplate(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET example.com/x/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	h := NewHandler(mux, WithTracerProvider(tp))
	req := httptest.NewRequest(http.MethodGet, "http://example.com/x/7", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)
	assert.Equal(t, "GET example.com/x/{id}", spans[0].Name())

	attrs := spanAttrSet(spans[0])
	for _, key := range []string{"url.path", "http.route"} {
		v, ok := attrs.Value(attribute.Key(key))
		require.True(t, ok, key)
		assert.Equal(t, "/x/{id}", v.AsString(), key)
	}
}

func TestNewHandler_ServerSpanRecordsTheRouteTemplateNotTheConcretePath(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	h := NewHandler(muxOn("GET /users/{id}"), WithTracerProvider(tp))
	serveOne(t, h, "/users/42?x=1", nil)

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	attrs := spanAttrSet(spans[0])

	path, ok := attrs.Value("url.path")
	require.True(t, ok, "the server span must still carry url.path")
	assert.Equal(t, "/users/{id}", path.AsString(), "url.path must be the route template")

	route, ok := attrs.Value("http.route")
	require.True(t, ok, "otelhttp must still record http.route")
	assert.Equal(t, "/users/{id}", route.AsString())

	// Ports are int64 attributes and a random test port could legitimately
	// contain "42", so only the string-valued attributes are scanned.
	for _, kv := range spans[0].Attributes() {
		if kv.Value.Type() != attribute.STRING {
			continue
		}

		assert.NotContains(t, kv.Value.AsString(), "42",
			"the concrete path id leaked into attribute %s", kv.Key)
	}
}

// Traffic that matched no route reports one stable template instead of one
// series per probed path, which is what keeps a scanner from inflating the
// attribute's cardinality.
func TestNewHandler_UnmatchedRequestRecordsTheUnmatchedTemplate(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	var patternSeen string

	// A bare handler (no ServeMux) never sets r.Pattern; the assertion below
	// proves it rather than assuming it.
	h := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patternSeen = r.Pattern

		w.WriteHeader(http.StatusOK)
	}), WithTracerProvider(tp))
	serveOne(t, h, "/anything/123.456.789-00", nil)

	assert.Empty(t, patternSeen, "no route matched, so r.Pattern must be empty")

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	attrs := spanAttrSet(spans[0])

	path, ok := attrs.Value("url.path")
	require.True(t, ok)
	assert.Equal(t, "/{unmatched}", path.AsString())

	// OpenTelemetry requires http.route to be ABSENT when nothing matched —
	// never the fallback template.
	assertNoKey(t, attrs, "http.route")
}

// The DEFAULT is unchanged: a private listener keeps the caller attributes it
// has always had. This is the control for the test below it.
func TestNewHandler_RecordsCallerAttributesByDefault(t *testing.T) {
	_, _, tp, sr := newHarness(t)

	h := NewHandler(muxOn("/users/{id}"), WithTracerProvider(tp))
	serveOne(t, h, "/users/1", http.Header{"User-Agent": []string{"probe/1.0"}})

	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	attrs := spanAttrSet(spans[0])
	assertHasKey(t, attrs, "user_agent.original")
	assertHasKey(t, attrs, "client.address")
}

// serverRequestBodySize returns the recorded sums of
// http.server.request.body.size, so a test can prove otelhttp's body wrapper
// survived the caller-attribute scrubbing.
func serverRequestBodySize(t *testing.T, reader *sdkmetric.ManualReader) []int64 {
	t.Helper()

	rm := &metricdata.ResourceMetrics{}
	require.NoError(t, reader.Collect(context.Background(), rm))

	var sums []int64

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "http.server.request.body.size" {
				continue
			}

			hist, ok := m.Data.(metricdata.Histogram[int64])
			require.True(t, ok, "expected int64 histogram, got %T", m.Data)

			for _, dp := range hist.DataPoints {
				sums = append(sums, dp.Sum)
			}
		}
	}

	return sums
}

// WithoutCallerAttributes keeps an attacker-controlled User-Agent /
// X-Forwarded-For and the caller's IP off a span that also carries this
// service's authenticated user ids — while the handler below still sees exactly
// what arrived, so access logs, rate limiters and IP allowlists are unaffected.
func TestNewHandler_WithoutCallerAttributesHidesTheCallerFromTheSpanOnly(t *testing.T) {
	mp, reader, tp, sr := newHarness(t)

	const (
		userAgent = "curl/8.7.1 <script>"
		forwarded = "203.0.113.7, 198.51.100.4"
		body      = "payload-bytes"
	)

	var (
		gotUserAgent  string
		gotForwarded  string
		gotRemoteAddr string
	)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/ingest", func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent = r.Header.Get("User-Agent")
		gotForwarded = r.Header.Get("X-Forwarded-For")
		gotRemoteAddr = r.RemoteAddr

		_, _ = io.Copy(io.Discard, r.Body)

		w.WriteHeader(http.StatusOK)
	})

	h := NewHandler(mux, WithTracerProvider(tp), WithMeterProvider(mp), WithoutCallerAttributes())

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, srv.URL+"/v1/ingest", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Forwarded-For", forwarded)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The span is blind to the caller.
	spans := serverSpans(t, sr)
	require.Len(t, spans, 1)

	attrs := spanAttrSet(spans[0])
	assertNoKey(t, attrs, "user_agent.original")
	assertNoKey(t, attrs, "client.address")
	assertNoKey(t, attrs, "network.peer.address")
	assertNoKey(t, attrs, "network.peer.port")

	for _, kv := range spans[0].Attributes() {
		v := kv.Value.Emit()
		assert.NotContains(t, v, "curl/", "the User-Agent leaked into attribute %s", kv.Key)
		assert.NotContains(t, v, "203.0.113.7", "the X-Forwarded-For leaked into attribute %s", kv.Key)
	}

	// The service's own host is NOT a caller attribute and must stay.
	assertHasKey(t, attrs, "server.address")

	// The handler below is not: it saw exactly what arrived.
	assert.Equal(t, userAgent, gotUserAgent, "the handler must receive the real User-Agent")
	assert.Equal(t, forwarded, gotForwarded, "the handler must receive the real X-Forwarded-For")
	assert.NotEmpty(t, gotRemoteAddr, "the handler must receive the real RemoteAddr")

	// Scrubbing must not cost the request-size accounting: the instrumented
	// request keeps otelhttp's body wrapper rather than being swapped out.
	sums := serverRequestBodySize(t, reader)
	require.NotEmpty(t, sums, "expected http.server.request.body.size to be emitted")
	assert.Equal(t, int64(len(body)), sums[0], "the request body bytes must still be counted")
}
