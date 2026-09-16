package httpobs

import (
	"net/http"
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
// a method-qualified route would be named "GET GET /v1/host". Both registration
// styles therefore yield the same name:
//
//	"GET /users/{id}"   -> "GET /users/{id}"
//	"/users/{id}"       -> "GET /users/{id}"
//	"GET example.com/x" -> "GET example.com/x"
//	""                  -> "GET"
func serverSpanName(_ string, r *http.Request) string {
	if r.Pattern == "" {
		return r.Method
	}

	route := r.Pattern
	if i := strings.IndexByte(route, ' '); i >= 0 {
		route = route[i+1:]
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

// NewTransport wraps base with OpenTelemetry HTTP client instrumentation: every
// outbound request is classified as an external-dependency call (SpanKind=CLIENT,
// only when a TracerProvider is configured — ADR-005), emits
// http.client.request.duration (seconds), and propagates trace context on the
// outbound headers.
//
// PREFER this wrapper for outbound HTTP. Use tracing.StartClientSpan only for
// outbound calls WITHOUT a wrapper. Do not double-instrument.
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

	return otelhttp.NewTransport(base, cfg.otelhttpOptions(boundedSpanName)...)
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
// otelhttp does not capture them and this wrapper adds nothing that would.
//
// Nil-safe like NewTransport: with no TracerProvider configured (neither option
// nor global) no span is produced, the metric degrades to no-op, and the request
// is still served.
func NewHandler(next http.Handler, opts ...Option) http.Handler {
	// An EMPTY composite propagator extracts nothing, so inbound trace context
	// is dropped unless the caller explicitly passes WithPropagators, which
	// comes later in the slice and therefore wins.
	resolved := append([]Option{WithPropagators(propagation.NewCompositeTextMapPropagator())}, opts...)

	cfg := newConfig(resolved...)

	return otelhttp.NewHandler(next, "", cfg.otelhttpOptions(serverSpanName)...)
}
