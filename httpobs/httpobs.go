package httpobs

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// config holds resolved helper options.
type config struct {
	meterProvider     metric.MeterProvider
	tracerProvider    trace.TracerProvider
	propagator        propagation.TextMapPropagator
	spanNameFormatter func(operation string, r *http.Request) string
}

// Option configures the HTTP client instrumentation helper.
type Option func(*config)

// WithMeterProvider sets the MeterProvider for http.client.request.duration.
// When unset the global provider is used (no-op unless configured). Nil ignored.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.meterProvider = mp
		}
	}
}

// WithTracerProvider sets the TracerProvider for the CLIENT span. When unset, no
// CLIENT span is produced (ADR-005): the telemetry-enabled path MUST pass this.
// Nil ignored.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		if tp != nil {
			c.tracerProvider = tp
		}
	}
}

// WithPropagators sets the propagator used to inject trace context into outbound
// request headers. When unset the global propagator is used. Nil ignored.
func WithPropagators(p propagation.TextMapPropagator) Option {
	return func(c *config) {
		if p != nil {
			c.propagator = p
		}
	}
}

// WithSpanNameFormatter overrides how the outbound span is named. The DEFAULT is
// bounded ("HTTP <METHOD>", e.g. "HTTP GET") and never includes the URL path.
// GUARDRAIL (docs/metrics-contract.md): a custom formatter MUST stay
// low-cardinality — never fold a concrete URL path / id / PII into the name.
func WithSpanNameFormatter(fn func(operation string, r *http.Request) string) Option {
	return func(c *config) {
		if fn != nil {
			c.spanNameFormatter = fn
		}
	}
}

func newConfig(opts ...Option) config {
	cfg := config{
		meterProvider: otel.GetMeterProvider(),
		propagator:    otel.GetTextMapPropagator(),
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// boundedSpanName is the default span name formatter. It is deliberately bounded
// to the HTTP method (e.g. "HTTP GET") and NEVER folds the URL path/query into
// the name (docs/metrics-contract.md: url.path with id/uuid is FORBIDDEN).
func boundedSpanName(_ string, r *http.Request) string {
	return "HTTP " + r.Method
}

// serverSpanName is the default span name formatter for INBOUND requests. It is
// bounded to the method plus the registered route PATTERN (r.Pattern, set by the
// Go 1.22+ http.ServeMux) and NEVER the concrete URL path, which can carry
// ids/PII. Falls back to the method alone when no pattern matched the request.
//
// A ServeMux pattern is "[METHOD ][HOST]/[PATH]", so a method-qualified pattern
// already carries the method ("GET /v1/host") and the space separates it. The
// method prefix is dropped before the name is built, otherwise every request on
// a method-qualified route would be named "GET GET /v1/host". ServeMux allows
// one or more spaces or tabs between the method and the rest of the pattern
// (net/http's pattern parser cuts on " \t" and trims the run), so the same
// separators are cut here. Every registration style therefore yields the same
// name:
//
//	"GET /users/{id}"   -> "GET /users/{id}"
//	"GET\t/users/{id}"  -> "GET /users/{id}"
//	"/users/{id}"       -> "GET /users/{id}"
//	"GET example.com/x" -> "GET example.com/x"
//	""                  -> "GET"
func serverSpanName(_ string, r *http.Request) string {
	if r.Pattern == "" {
		return r.Method
	}

	route := r.Pattern
	if i := strings.IndexAny(route, " \t"); i >= 0 {
		route = strings.TrimLeft(route[i+1:], " \t")
	}

	return r.Method + " " + route
}

// otelhttpOptions translates the resolved config into otelhttp options, using
// defaultSpanName when the caller supplied no formatter of its own.
func (c config) otelhttpOptions(defaultSpanName func(operation string, r *http.Request) string) []otelhttp.Option {
	opts := []otelhttp.Option{
		otelhttp.WithMeterProvider(c.meterProvider),
		otelhttp.WithPropagators(c.propagator),
	}

	// ADR-005: only attach a TracerProvider (and thus produce a CLIENT span) when
	// one was supplied; otherwise the wrapper degrades to metric-only / no-op.
	if c.tracerProvider != nil {
		opts = append(opts, otelhttp.WithTracerProvider(c.tracerProvider))
	}

	// GUARDRAIL: always enforce a bounded span name. Use the caller's formatter
	// when given, else the method-only default — never the otelhttp default,
	// which could change and fold the path into the name.
	formatter := c.spanNameFormatter
	if formatter == nil {
		formatter = defaultSpanName
	}

	opts = append(opts, otelhttp.WithSpanNameFormatter(formatter))

	return opts
}

// originalURLKey addresses the untouched request URL that scrubbedURLTransport
// stashes on the context and restoredURLTransport reads back. Private type, so
// nothing outside this package can collide with it or read it.
type originalURLKey struct{}

// scrubbedURLTransport is the OUTER half of the credential guarantee: the
// instrumentation below it only ever sees scheme://host/path (scheme://host when
// the target was opaque, since the path lived inside Opaque).
//
// otelhttp reads req.URL when it STARTS the span, inside the same RoundTrip that
// performs the request, so url.full cannot be filtered after the fact — the
// request handed to the instrumentation must already be clean. The untouched URL
// travels on the context and restoredURLTransport puts it back on the request
// otelhttp builds, one layer lower, so the wire is unaffected.
type scrubbedURLTransport struct {
	instrumented http.RoundTripper
}

func (t scrubbedURLTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL == nil ||
		(r.URL.RawQuery == "" && !r.URL.ForceQuery && r.URL.Fragment == "" &&
			r.URL.User == nil && r.URL.Opaque == "") {
		// Nothing a credential could hide in: spend no clone on it.
		return t.instrumented.RoundTrip(r)
	}

	// A RoundTripper must not modify the request it was given, and Clone deep
	// copies the URL (net/http cloneURL), so the caller's URL is untouched.
	// Opaque is cleared too: when set, URL.String() prints it verbatim, query
	// and all, and the fields below are never consulted.
	clone := r.Clone(context.WithValue(r.Context(), originalURLKey{}, r.URL))
	clone.URL.User = nil
	clone.URL.Opaque = ""
	clone.URL.RawQuery = ""
	clone.URL.ForceQuery = false
	clone.URL.Fragment = ""
	clone.URL.RawFragment = ""

	return t.instrumented.RoundTrip(clone)
}

// restoredURLTransport is the INNER half: it sits between the instrumentation
// and the application's real transport and gives the request its full URL back,
// so query string, fragment, userinfo and an opaque target still reach the wire.
type restoredURLTransport struct {
	base http.RoundTripper
}

func (t restoredURLTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// otelhttp already cloned the request before handing it down ("r =
	// r.Clone(ctx). According to RoundTripper spec, we shouldn't modify the
	// origin request." — otelhttp transport.go), so this assignment touches
	// neither the caller's request nor the one scrubbedURLTransport built. The
	// traceparent otelhttp injected and the body wrapper it installed for byte
	// counting ride along untouched; only the URL changes.
	if original, ok := r.Context().Value(originalURLKey{}).(*url.URL); ok {
		r.URL = original
	}

	return t.base.RoundTrip(r)
}

// NewTransport wraps base with OpenTelemetry HTTP client instrumentation: every
// outbound request is classified as an external-dependency call (SpanKind=CLIENT,
// only when a TracerProvider is configured — ADR-005), emits
// http.client.request.duration (seconds), and propagates trace context on the
// outbound headers.
//
// PREFER this wrapper for outbound HTTP. Use tracing.StartClientSpan only for
// outbound calls WITHOUT a wrapper. Do not double-instrument.
//
// # Credentials in the URL (always, no opt-out)
//
// The URL recorded on the span (url.full) never carries query string, fragment,
// userinfo or an opaque target — all removed before the instrumentation sees the
// request; a hierarchical URL keeps scheme://host/path, an opaque one only
// scheme://host. An API key in the query (?key=, ?token=, a pre-signed S3/GCS
// signature) would otherwise be exported verbatim to the collector. The request
// on the wire is UNCHANGED — the full URL is restored below the instrumentation.
//
// Nil-safe: base == nil uses http.DefaultTransport. With no providers configured
// it attaches against the no-op providers, so telemetry being off never breaks
// the client. The span ends when the response body is fully read/closed — the
// caller MUST read and close the body.
func NewTransport(base http.RoundTripper, opts ...Option) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}

	cfg := newConfig(opts...)

	instrumented := otelhttp.NewTransport(
		restoredURLTransport{base: base},
		cfg.otelhttpOptions(boundedSpanName)...,
	)

	return scrubbedURLTransport{instrumented: instrumented}
}

// NewClient returns an *http.Client whose Transport is the instrumented wrapper
// around base. Passing base preserves the caller's custom transport (TLS /
// timeout / proxy). base == nil uses http.DefaultTransport.
func NewClient(base http.RoundTripper, opts ...Option) *http.Client {
	return &http.Client{Transport: NewTransport(base, opts...)}
}

// NewHandler wraps next with OpenTelemetry HTTP SERVER instrumentation: every
// inbound request produces a SpanKind=SERVER span and emits
// http.server.request.duration (seconds). It is the inbound counterpart of
// NewTransport, for a stdlib net/http server (including one served over a unix
// socket); a Fiber v3 app uses middleware.WithTelemetry instead. Do not register
// both on the same server - that records the duration histogram twice.
//
// # Span name (bounded by default)
//
// The default name is the method plus the registered route PATTERN - "GET
// /users/{id}" - taken from r.Pattern, which the Go 1.22+ http.ServeMux sets;
// with no pattern it is the method alone. A method-qualified registration
// ("GET /users/{id}") names the span exactly like a bare one ("/users/{id}"):
// the pattern's own method prefix is dropped rather than repeated. The concrete
// URL path never enters the name. WithSpanNameFormatter overrides it and, per
// docs/metrics-contract.md, MUST stay low-cardinality.
//
// # Inbound trace context (fail-closed by default)
//
// By DEFAULT the inbound traceparent/tracestate headers are IGNORED and every
// request starts a NEW ROOT trace: a caller able to set traceparent otherwise
// chooses the trace id this service records under and can force its sampling
// decision. Pass otel.GetTextMapPropagator() (or any explicit propagator) via
// WithPropagators to continue a trusted caller's trace. That is the same trust
// decision tracing.TelemetryConfig.TrustInboundTraceContext expresses for the
// Fiber and gRPC paths, spelled here as the propagator you hand in instead of a
// second knob.
//
// # Payloads
//
// Request and response bodies and the Authorization header are NEVER recorded:
// otelhttp does not capture them and this wrapper adds nothing that would. The
// QUERY STRING is not recorded either — the SERVER span carries url.path and
// never a full URL, so an OAuth callback's ?code=/?state= stays out of the
// trace. The handler below still receives the request whole, query and all.
//
// Degrades like NewTransport when telemetry is absent: with no TracerProvider
// configured (neither option nor global) no span is produced, the metric
// degrades to no-op, and the request is still served. That is the whole of the
// guarantee — a nil or panicking wrapped handler is the caller's, as with any
// middleware; otelhttp calls next.ServeHTTP directly and recovers nothing.
func NewHandler(next http.Handler, opts ...Option) http.Handler {
	// An EMPTY composite propagator extracts nothing, so inbound trace context
	// is dropped unless the caller explicitly passes WithPropagators, which
	// comes later in the slice and therefore wins.
	resolved := append([]Option{WithPropagators(propagation.NewCompositeTextMapPropagator())}, opts...)

	cfg := newConfig(resolved...)

	return otelhttp.NewHandler(next, "", cfg.otelhttpOptions(serverSpanName)...)
}
