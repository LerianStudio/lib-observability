package httpobs

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/LerianStudio/lib-observability/v4/constants"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
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

	withoutCallerAttributes bool
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

// WithoutCallerAttributes keeps the CALLER off the SERVER span: no
// user_agent.original, no client.address, no network.peer.address /
// network.peer.port. OFF by default.
//
// Turn it on for a PUBLIC listener. Those four attributes are sourced from the
// User-Agent header, the X-Forwarded-For header and the peer address, i.e. from
// values an unauthenticated caller chooses, onto a span that also carries this
// service's authenticated user and tenant ids. Off a public listener that makes
// the trace both an amplification surface (a header of arbitrary length and
// cardinality, exported verbatim) and a correlation of an attacker-supplied
// string with an identified user.
//
// The HANDLER below is unaffected: it receives the User-Agent, the
// X-Forwarded-For and the RemoteAddr that actually arrived, so access logs,
// rate limiters and IP allowlists see the real caller. Only the instrumentation
// is blinded. server.address (this service's own host) is kept either way.
func WithoutCallerAttributes() Option {
	return func(c *config) {
		c.withoutCallerAttributes = true
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

	return r.Method + " " + routePath(r.Pattern)
}

// routePath cuts a ServeMux pattern's optional "METHOD " prefix, leaving the
// "[HOST]/[PATH]" part — the route template. ServeMux allows one or more spaces
// or tabs between the method and the rest (net/http's pattern parser cuts on
// " \t" and trims the run), so the same separators are cut here.
func routePath(pattern string) string {
	if i := strings.IndexAny(pattern, " \t"); i >= 0 {
		return strings.TrimLeft(pattern[i+1:], " \t")
	}

	return pattern
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

// Header names scrubbed and restored by WithoutCallerAttributes. Both are
// already in canonical MIME form, so the header map can be indexed directly —
// which preserves a repeated X-Forwarded-For that Header.Set would flatten.
const (
	userAgentHeader    = "User-Agent"
	forwardedForHeader = "X-Forwarded-For"
)

// callerAttrsKey addresses the caller identity that scrubCallerAttributes
// stashes on the context and restoreCallerAttributes reads back. Private type,
// so nothing outside this package can collide with it or read it.
type callerAttrsKey struct{}

// callerAttrs is what actually arrived, held aside while the instrumentation
// looks at the request.
type callerAttrs struct {
	userAgent    []string
	forwardedFor []string
	remoteAddr   string
}

// scrubCallerAttributes is the OUTER half of WithoutCallerAttributes: the
// instrumentation below it sees a request with no User-Agent, no
// X-Forwarded-For and an empty RemoteAddr, so otelhttp's SERVER semconv records
// none of user_agent.original, client.address, network.peer.address or
// network.peer.port (internal/semconv/server.go sources all four from exactly
// those three fields, and omits each attribute when its source is empty).
//
// otelhttp reads them when it STARTS the span, before the handler runs, so they
// cannot be filtered after the fact — the request handed to the instrumentation
// must already be blind. Clone deep-copies the header map, so the server's own
// request is untouched; the real values travel on the context and
// restoreCallerAttributes puts them back one layer lower.
func scrubCallerAttributes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		saved := callerAttrs{
			userAgent:    r.Header[userAgentHeader],
			forwardedFor: r.Header[forwardedForHeader],
			remoteAddr:   r.RemoteAddr,
		}

		clone := r.Clone(context.WithValue(r.Context(), callerAttrsKey{}, saved))
		clone.Header.Del(userAgentHeader)
		clone.Header.Del(forwardedForHeader)
		clone.RemoteAddr = ""

		next.ServeHTTP(w, clone)
	})
}

// restoreCallerAttributes is the INNER half: it sits between the
// instrumentation and the application's handler and gives the request its
// caller identity back, so access logs, rate limiters and IP allowlists below
// still see what actually arrived.
//
// otelhttp hands down r.WithContext(ctx) — a shallow copy sharing the clone's
// header map — so writing here is what the next handler reads. The request
// itself is deliberately NOT swapped back for the original: otelhttp wrapped
// its Body to count http.server.request.body.size, and replacing the request
// would throw that wrapper away.
func restoreCallerAttributes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if saved, ok := r.Context().Value(callerAttrsKey{}).(callerAttrs); ok {
			if saved.userAgent != nil {
				r.Header[userAgentHeader] = saved.userAgent
			}

			if saved.forwardedFor != nil {
				r.Header[forwardedForHeader] = saved.forwardedFor
			}

			r.RemoteAddr = saved.remoteAddr
		}

		next.ServeHTTP(w, r)
	})
}

// routeTemplateAttribute is the inbound counterpart of the outbound URL
// guarantee: it replaces the CONCRETE url.path otelhttp recorded at span start
// with the route TEMPLATE, so "/users/42" is exported as "/users/{id}" and an
// id or PII in the path never leaves the process. Traffic that matched no route
// reports one stable constants.UnmatchedRouteTemplate instead of one series per
// probed path.
//
// It must run BELOW the instrumentation and AFTER the handler: r.Pattern is set
// by http.ServeMux in place, on the very request pointer otelhttp still holds
// (net/http server.go: "h, r.Pattern, r.pat, r.matches = mux.findHandler(r)"),
// so it is only readable once the mux has routed. SetAttributes on a key the
// span already carries replaces the value — the SDK appends and deduplicates on
// read, keeping the LAST write (sdk/trace/span.go dedupeAttrsFromRecord:
// "unique[idx] = a"), and the export path deduplicates in snapshot() — so the
// concrete path is overwritten, never exported alongside.
//
// http.route rides along for the same reason and from the same pattern. otelhttp
// derives it too, but only for the METRIC: it builds the span's attributes
// before the handler runs (internal/semconv/server.go RequestTraceAttrs), when
// r.Pattern is still empty, and afterwards re-reads the pattern only to rename
// the span. So without this the SERVER span carries no http.route at all. It is
// omitted — never "/{unmatched}" — when nothing matched, as OpenTelemetry
// requires and as the Fiber middleware already does.
func routeTemplateAttribute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)

		span := trace.SpanFromContext(r.Context())

		if r.Pattern == "" {
			span.SetAttributes(attribute.String("url.path", constants.UnmatchedRouteTemplate))

			return
		}

		route := routePath(r.Pattern)
		span.SetAttributes(
			attribute.String("url.path", route),
			attribute.String("http.route", route),
		)
	})
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
// # url.path is the route template (always, no opt-out)
//
// url.path on the SERVER span is the route TEMPLATE, never the concrete path:
// a request to /users/42 is exported as url.path="/users/{id}", and traffic
// that matched no route as url.path="/{unmatched}". So a customer id, a CPF or
// an account number sitting in the path never leaves the process, and the
// attribute stays low-cardinality. This mirrors what middleware.WithTelemetry
// does on the Fiber path. http.route is set from the same pattern (otelhttp
// puts it only on the metric, never on the span) and is OMITTED — never the
// fallback template — when nothing matched, as OpenTelemetry requires.
//
// # Caller identity (opt-in removal)
//
// By DEFAULT the span carries user_agent.original, client.address and
// network.peer.address/port — the caller's own User-Agent, X-Forwarded-For and
// peer address. WithoutCallerAttributes removes all four, for a PUBLIC
// listener whose spans also carry authenticated user ids; the handler below
// still sees the real values. server.address (this service's own host) is kept
// either way.
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

	// Both wrappers must sit BELOW the instrumentation: url.path can only be
	// rewritten once the mux has set r.Pattern, and the caller identity can only
	// be given back after otelhttp has read the request.
	inner := routeTemplateAttribute(next)
	if cfg.withoutCallerAttributes {
		inner = restoreCallerAttributes(inner)
	}

	instrumented := otelhttp.NewHandler(inner, "", cfg.otelhttpOptions(serverSpanName)...)
	if cfg.withoutCallerAttributes {
		return scrubCallerAttributes(instrumented)
	}

	return instrumented
}
