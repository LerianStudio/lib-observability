package httpobs

import (
	"net/http"

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

// otelhttpOptions translates the resolved config into otelhttp options.
func (c config) otelhttpOptions() []otelhttp.Option {
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
		formatter = boundedSpanName
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

	return otelhttp.NewTransport(base, cfg.otelhttpOptions()...)
}

// NewClient returns an *http.Client whose Transport is the instrumented wrapper
// around base. Passing base preserves the caller's custom transport (TLS /
// timeout / proxy). base == nil uses http.DefaultTransport.
func NewClient(base http.RoundTripper, opts ...Option) *http.Client {
	return &http.Client{Transport: NewTransport(base, opts...)}
}
