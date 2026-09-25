package tracing

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	observability "github.com/LerianStudio/lib-observability/v4"
	"github.com/LerianStudio/lib-observability/v4/assert"
	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/metrics"
	"github.com/google/uuid"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

const (
	maxSpanAttributeStringLength = 4096
	maxAttributeDepth            = 32
	maxAttributeCount            = 128
	defaultAttrPrefix            = "value"
)

var (
	// ErrNilTelemetryLogger is returned when telemetry config has no logger.
	ErrNilTelemetryLogger = errors.New("telemetry config logger cannot be nil")
	// ErrEmptyEndpoint is returned when telemetry is enabled without exporter endpoint.
	ErrEmptyEndpoint = errors.New("collector exporter endpoint cannot be empty when telemetry is enabled")
	// ErrNilTelemetry is returned when a telemetry method receives a nil receiver.
	ErrNilTelemetry = errors.New("telemetry instance is nil")
	// ErrNilShutdown is returned when telemetry shutdown handlers are unavailable.
	ErrNilShutdown = errors.New("telemetry shutdown function is nil")
	// ErrNilProvider is returned when ApplyGlobals is called with nil providers.
	ErrNilProvider = errors.New("telemetry providers must not be nil when applying globals")
	// ErrInvalidSampleRatio is returned when TelemetryConfig.SampleRatio is
	// outside the accepted range: 0 (unset) or (0, 1].
	ErrInvalidSampleRatio = errors.New("telemetry sample ratio must be 0 (unset) or within (0, 1]")
)

// TelemetryConfig configures tracing, metrics, logging, and propagation behavior.
type TelemetryConfig struct {
	// LibraryName is the instrumentation scope of the signals the service
	// emits through this library: the business metrics declared on
	// Telemetry.MetricsFactory and the spans opened with the tracer the HTTP
	// and gRPC middleware put on the request context. It is used exactly as
	// configured: no trimming and no fallback to ServiceName, so empty means
	// an empty scope. It is never this library's module path, whose version
	// changes on every library release.
	//
	// Signals this library emits itself (middleware, interceptor and
	// messaging spans, the transport duration instruments) ignore it and
	// carry the library's own module scope.
	LibraryName    string
	ServiceName    string
	ServiceVersion string
	// ServiceRevision is the full git SHA the binary was built from, set at
	// link time (-ldflags "-X main.revision=..."). When set, it is published on the
	// OTel resource as vcs.ref.head.revision; empty or whitespace-only means
	// the attribute is omitted. Surrounding whitespace is trimmed before the
	// value is published, so a padded SHA still matches an exact-match
	// dashboard query. No other format validation happens here.
	ServiceRevision string
	DeploymentEnv   string
	// ServiceInstanceID is the OpenTelemetry service.instance.id resource
	// attribute: the identity that tells one replica of a service apart from
	// the others, so traces, metrics and logs can be read per instance. It is
	// optional. Precedence: this field, when non-empty, wins; otherwise a
	// service.instance.id supplied through OTEL_RESOURCE_ATTRIBUTES is used;
	// otherwise a random UUID v4 is generated once per NewTelemetry call, as
	// the OpenTelemetry semantic conventions recommend. A generated id is
	// therefore stable for the life of one Telemetry instance and different
	// on every process start.
	//
	// The other attributes OTEL_RESOURCE_ATTRIBUTES carries (for example
	// k8s.pod.name and k8s.namespace.name injected through the Kubernetes
	// downward API) are added to the resource too. ServiceName,
	// ServiceVersion and DeploymentEnv follow the same rule as this field: a
	// non-empty value wins over OTEL_SERVICE_NAME and over the same key in
	// OTEL_RESOURCE_ATTRIBUTES; an empty one is unset and takes the
	// environment's value; when neither supplies one the attribute is omitted
	// rather than published as an empty string.
	ServiceInstanceID         string
	CollectorExporterEndpoint string
	EnableTelemetry           bool
	InsecureExporter          bool
	// EnableRuntimeMetrics turns on the Go runtime instrumentation
	// (go.opentelemetry.io/contrib/instrumentation/runtime), emitting go.*
	// runtime metrics (heap, GC, goroutines) through this instance's
	// MeterProvider. It follows the Go zero-value convention: default false,
	// opt-in true. It is honored only when EnableTelemetry is also true and a
	// real MeterProvider exists; with telemetry disabled or a noop provider it
	// degrades to a no-op. Applications that want runtime metrics on by default
	// should set this to true explicitly at their bootstrap - keeping the
	// zero-value off avoids surprising callers who construct TelemetryConfig
	// partially and inheriting a background collector they did not request.
	EnableRuntimeMetrics bool
	// TrustInboundTraceContext controls whether inbound trace-context
	// extraction (a W3C traceparent/tracestate header, over HTTP or gRPC)
	// continues that trace, instead of starting a fresh root span for every
	// request. It follows the Go zero-value convention: default false
	// (fail-closed), opt-in true. Shared, not per-transport: setting it once
	// on a Telemetry instance governs both the HTTP middleware
	// (middleware.WithTelemetry) and the gRPC server interceptor
	// (grpcmiddleware.WithTelemetryInterceptor) consistently. Earlier, gRPC
	// had its own, separate gate (a User-Agent heuristic, spoofable by any
	// caller that set the header, so never a real trust boundary) - it has
	// been replaced by this flag; a service behind a trusted internal mesh
	// that relied on that heuristic now needs to opt in here explicitly to
	// keep joining inbound gRPC traces.
	//
	// With an untrusted caller able to set traceparent, extracting it
	// unconditionally lets that caller choose the trace ID this service
	// records under and, depending on the configured sampler, force a
	// sampling decision via the header's sampled flag - both are ways an
	// external request can manipulate this service's own telemetry. Set this
	// to true only for a service that sits behind a trusted boundary which
	// itself generates or validates the header (an internal service mesh, a
	// trusted API gateway) - never for a service directly reachable by
	// external clients.
	//
	// The tenant.id BAGGAGE member extracted alongside trace context is
	// ALWAYS stripped regardless of this flag, and regardless of transport -
	// the strip lives in the shared ExtractTraceContext funnel every
	// extraction path (HTTP, gRPC, queue) goes through, see its doc comment.
	// This is narrower than "tenant identity never comes from a
	// caller-controlled field" - it covers baggage specifically.
	// grpcmiddleware.ResolveTenantIDFromGRPC is a separate, pre-existing
	// mechanism that DOES read a caller-controlled `tenant-id` gRPC metadata
	// field for span/metric labeling; see its own doc comment for that gap.
	TrustInboundTraceContext bool
	// SampleRatio is the head sampling probability applied to a trace that
	// arrives with no sampled parent. It follows the Go zero-value convention:
	// 0 means UNSET and keeps the SDK default, ParentBased(AlwaysSample), so
	// every root trace is recorded - the behavior every caller had before this
	// field existed. A value in (0, 1] installs
	// sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio)): a root trace is
	// recorded with that probability, while a request arriving with an
	// already-sampled parent still follows the parent's decision, so a trace is
	// never truncated halfway through. Any other value - negative, greater than
	// 1, or NaN - makes NewTelemetry return ErrInvalidSampleRatio before any
	// provider is built.
	//
	// It is honored only on the real provider path. With telemetry disabled, or
	// with an empty collector endpoint, the noop providers ignore it.
	SampleRatio float64
	// Logger is typed log.Universal - a single Log method built from universal
	// types - rather than log.Logger, so a service can populate this config
	// with a logger declared in its own package (or in a library that has
	// decoupled from lib-observability) with no adapter. Any log.Logger
	// satisfies it, so existing callers are unaffected.
	Logger     log.Universal
	Propagator propagation.TextMapPropagator
	Redactor   *Redactor
}

// TelemetryOption configures optional provider behavior without extending
// TelemetryConfig. Keeping these settings separate preserves source
// compatibility for callers that use unkeyed TelemetryConfig literals.
type TelemetryOption interface {
	apply(opts *telemetryOptions)
}

type telemetryOptions struct {
	metricCardinalityLimit int
}

type telemetryOptionFunc func(*telemetryOptions)

func (fn telemetryOptionFunc) apply(opts *telemetryOptions) {
	fn(opts)
}

// WithMetricCardinalityLimit caps how many distinct attribute sets the metrics
// SDK retains per instrument. A value less than or equal to zero keeps the
// OpenTelemetry SDK default (2000); a positive value overrides it.
//
// Past the limit the SDK collapses excess attribute sets into a single
// otel.metric.overflow=true data point and discards their original attributes.
// Budget the limit per instrument. For the per-tenant counters, whose attribute
// set is tenant.id x http.route, the tenant ceiling is
// floor((limit - 1) / normalized routes) because the SDK reserves one set for
// overflow.
func WithMetricCardinalityLimit(limit int) TelemetryOption {
	return telemetryOptionFunc(func(opts *telemetryOptions) {
		opts.metricCardinalityLimit = limit
	})
}

// Telemetry holds configured OpenTelemetry providers and lifecycle handlers.
type Telemetry struct {
	TelemetryConfig
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	LoggerProvider *sdklog.LoggerProvider
	MetricsFactory *metrics.MetricsFactory
	shutdown       func()
	shutdownCtx    func(context.Context) error
}

// NewTelemetry builds telemetry providers and exporters from configuration.
func NewTelemetry(cfg TelemetryConfig) (*Telemetry, error) {
	return newTelemetry(cfg, telemetryOptions{})
}

// NewTelemetryWithOptions builds telemetry providers and exporters from
// configuration plus optional provider settings. NewTelemetry remains the
// source-compatible default entry point.
func NewTelemetryWithOptions(cfg TelemetryConfig, options ...TelemetryOption) (*Telemetry, error) {
	resolved := telemetryOptions{}

	for _, option := range options {
		if option != nil {
			option.apply(&resolved)
		}
	}

	return newTelemetry(cfg, resolved)
}

func newTelemetry(cfg TelemetryConfig, options telemetryOptions) (*Telemetry, error) {
	if log.IsNil(cfg.Logger) {
		return nil, ErrNilTelemetryLogger
	}

	if err := validateSampleRatio(cfg.SampleRatio); err != nil {
		return nil, err
	}

	if cfg.Propagator == nil {
		cfg.Propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	}

	if cfg.Redactor == nil {
		cfg.Redactor = NewDefaultRedactor()
	}

	normalizeEndpoint(&cfg)
	normalizeEndpointEnvVars(cfg.Logger, cfg.InsecureExporter)

	if cfg.EnableTelemetry && strings.TrimSpace(cfg.CollectorExporterEndpoint) == "" {
		return handleEmptyEndpoint(cfg)
	}

	ctx := context.Background()

	if !cfg.EnableTelemetry {
		cfg.Logger.Log(ctx, log.LevelWarn, "Telemetry disabled")

		return newNoopTelemetry(cfg)
	}

	normalizedDeploymentEnv := strings.ToLower(strings.TrimSpace(cfg.DeploymentEnv))
	if cfg.InsecureExporter && normalizedDeploymentEnv != "" &&
		normalizedDeploymentEnv != "development" && normalizedDeploymentEnv != "local" {
		cfg.Logger.Log(ctx, log.LevelWarn,
			"InsecureExporter is enabled in non-development environment",
			log.String("environment", normalizedDeploymentEnv))
	}

	// Security policy: insecure OTEL exporter enforcement in production.
	// In production environments, InsecureExporter must not be used unless
	// the ALLOW_INSECURE_OTEL env var is set with a justification.
	if cfg.InsecureExporter && isStrictEnvironment(normalizedDeploymentEnv) {
		if os.Getenv("ALLOW_INSECURE_OTEL") == "" {
			return nil, fmt.Errorf("otel new: insecure exporter detected in %q environment (use https:// endpoint or set ALLOW_INSECURE_OTEL=\"reason\")",
				normalizedDeploymentEnv)
		}
	}

	return initExporters(ctx, cfg, options)
}

// validateSampleRatio rejects a SampleRatio outside 0 (unset) or (0, 1]. NaN
// fails both comparisons and is therefore rejected as well.
func validateSampleRatio(ratio float64) error {
	if ratio == 0 || (ratio > 0 && ratio <= 1) {
		return nil
	}

	return fmt.Errorf("%w: got %v", ErrInvalidSampleRatio, ratio)
}

// normalizeEndpoint strips URL scheme from the collector endpoint and infers security mode.
// gRPC WithEndpoint() expects host:port, not a full URL.
// Consumers commonly pass OTEL_EXPORTER_OTLP_ENDPOINT as "http://host:4317".
func normalizeEndpoint(cfg *TelemetryConfig) {
	ep := strings.TrimSpace(cfg.CollectorExporterEndpoint)
	if ep == "" {
		return
	}

	switch {
	case strings.HasPrefix(ep, "http://"):
		cfg.CollectorExporterEndpoint = strings.TrimPrefix(ep, "http://")
		cfg.InsecureExporter = true
	case strings.HasPrefix(ep, "https://"):
		cfg.CollectorExporterEndpoint = strings.TrimPrefix(ep, "https://")
	default:
		// No scheme — assume insecure (common in k8s internal comms).
		// Persist the trimmed value back so leading/trailing whitespace is dropped.
		cfg.CollectorExporterEndpoint = ep
		cfg.InsecureExporter = true
	}
}

// normalizeEndpointEnvVars ensures OTEL exporter endpoint environment variables
// contain a URL scheme. The OTEL SDK's envconfig reads these via url.Parse(),
// which fails on bare "host:port" values, so adding a scheme prevents noisy
// "parse url" errors from the SDK's internal logger. The scheme follows
// InsecureExporter — "https://" for a secure exporter, "http://" for an
// insecure one — so the environment the SDK reads agrees with the connection
// the library makes.
//
// It mutates the calling process's environment via os.Setenv: anything that
// re-reads these variables later sees the normalized value.
func normalizeEndpointEnvVars(logger log.Universal, insecure bool) {
	scheme := "https://"
	if insecure {
		scheme = "http://"
	}

	for _, key := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" || strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
			continue
		}

		// Failure here means the SDK will later choke on the bare host:port value,
		// so surface it rather than swallowing it silently.
		if err := os.Setenv(key, scheme+v); err != nil && logger != nil {
			logger.Log(context.Background(), log.LevelWarn,
				"failed to normalize OTEL endpoint env var",
				log.String("key", key), log.Err(err))
		}
	}
}

// handleEmptyEndpoint handles the case where telemetry is enabled but the collector
// endpoint is empty, returning noop providers installed as globals.
func handleEmptyEndpoint(cfg TelemetryConfig) (*Telemetry, error) {
	cfg.Logger.Log(context.Background(), log.LevelWarn,
		"Telemetry enabled but collector endpoint is empty; falling back to noop providers")

	tl, noopErr := newNoopTelemetry(cfg)
	if noopErr != nil {
		return nil, noopErr
	}

	// Set noop providers as globals so downstream libraries (e.g. otelfiber)
	// do not create real gRPC exporters that leak background goroutines.
	_ = tl.ApplyGlobals()

	return tl, ErrEmptyEndpoint
}

// initExporters creates OTLP exporters, providers, and a metrics factory,
// rolling back partial allocations on failure.
func initExporters(ctx context.Context, cfg TelemetryConfig, options telemetryOptions) (*Telemetry, error) {
	tExp, mExp, lExp, err := cfg.newExporters(ctx)
	if err != nil {
		return nil, err
	}

	return buildTelemetry(ctx, cfg, options, tExp, mExp, lExp)
}

// newExporters creates the three OTLP gRPC exporters, shutting down the ones
// already built when a later one fails. Nothing owns them yet at this point,
// so they are the only thing there is to roll back.
func (tl *TelemetryConfig) newExporters(ctx context.Context) (
	sdktrace.SpanExporter, sdkmetric.Exporter, sdklog.Exporter, error,
) {
	var built []shutdownable

	tExp, err := tl.newTracerExporter(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("can't initialize tracer exporter: %w", err)
	}

	built = append(built, tExp)

	mExp, err := tl.newMetricExporter(ctx)
	if err != nil {
		shutdownAll(ctx, built)

		return nil, nil, nil, fmt.Errorf("can't initialize metric exporter: %w", err)
	}

	built = append(built, mExp)

	lExp, err := tl.newLoggerExporter(ctx)
	if err != nil {
		shutdownAll(ctx, built)

		return nil, nil, nil, fmt.Errorf("can't initialize logger exporter: %w", err)
	}

	return tExp, mExp, lExp, nil
}

// buildTelemetry wires the providers over the supplied exporters and assembles
// the Telemetry instance, rolling back the providers if a later step fails.
//
// Each provider OWNS the exporter it is handed: sdktrace's batcher, sdkmetric's
// PeriodicReader and sdklog's BatchProcessor each call Shutdown on the exporter
// as part of the provider's own Shutdown. So from here on the exporters are
// deliberately absent from both the rollback list and the shutdown handlers -
// draining one a second time is what makes otlpmetricgrpc answer "gRPC exporter
// is shutdown" and turns a clean process exit into a reported drain failure.
func buildTelemetry(
	ctx context.Context,
	cfg TelemetryConfig,
	options telemetryOptions,
	tExp sdktrace.SpanExporter,
	mExp sdkmetric.Exporter,
	lExp sdklog.Exporter,
) (*Telemetry, error) {
	r, err := cfg.newResource(ctx)
	if err != nil {
		// No provider owns the exporters yet, so they are what rolls back.
		shutdownAll(ctx, []shutdownable{tExp, mExp, lExp})

		return nil, fmt.Errorf("can't initialize resource: %w", err)
	}

	mp := cfg.newMeterProvider(r, mExp, options.metricCardinalityLimit)
	tp := cfg.newTracerProvider(r, tExp)
	lp := cfg.newLoggerProvider(r, lExp)

	providers := []shutdownable{mp, tp, lp}

	metricsFactory, err := metrics.NewMetricsFactory(mp.Meter(cfg.LibraryName), cfg.Logger)
	if err != nil {
		shutdownAll(ctx, providers)

		return nil, err
	}

	// Start Go runtime instrumentation on the real MeterProvider when opted in.
	// Best-effort: a failure here is logged inside the helper and never aborts
	// telemetry bring-up, since runtime metrics are auxiliary to request-path
	// observability.
	startRuntimeMetrics(cfg, mp)

	shutdown, shutdownCtx := buildShutdownHandlers(cfg.Logger, providers...)

	return &Telemetry{
		TelemetryConfig: cfg,
		TracerProvider:  tp,
		MeterProvider:   mp,
		LoggerProvider:  lp,
		MetricsFactory:  metricsFactory,
		shutdown:        shutdown,
		shutdownCtx:     shutdownCtx,
	}, nil
}

// runtimeMinReadMemStatsInterval is the minimum interval between the relatively
// expensive runtime.ReadMemStats() calls made by the Go runtime instrumentation.
const runtimeMinReadMemStatsInterval = 15 * time.Second

// startRuntimeMetrics registers the Go runtime instrumentation
// (go.opentelemetry.io/contrib/instrumentation/runtime) against the supplied
// MeterProvider when cfg.EnableRuntimeMetrics is set. It returns true when the
// instrumentation was started, false when it was skipped (toggle off, nil
// MeterProvider) or failed to register.
//
// It is best-effort and never panics: a nil MeterProvider or a Start error is
// logged (when a logger is available) and reported via the false return, so
// telemetry bring-up proceeds regardless. The contrib instrumentation registers
// asynchronous callbacks on the MeterProvider's meter; there is no separate
// goroutine to shut down, so teardown follows the MeterProvider's own shutdown.
func startRuntimeMetrics(cfg TelemetryConfig, mp *sdkmetric.MeterProvider) bool {
	if !cfg.EnableRuntimeMetrics {
		return false
	}

	if mp == nil {
		if cfg.Logger != nil {
			cfg.Logger.Log(context.Background(), log.LevelWarn,
				"runtime metrics requested but MeterProvider is nil; skipping")
		}

		return false
	}

	err := otelruntime.Start(
		otelruntime.WithMeterProvider(mp),
		otelruntime.WithMinimumReadMemStatsInterval(runtimeMinReadMemStatsInterval),
	)
	if err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Log(context.Background(), log.LevelError,
				"failed to start Go runtime metrics", log.Err(err))
		}

		return false
	}

	return true
}

// newNoopTelemetry creates a Telemetry instance with no-op providers (no exporters).
// This is used when telemetry is disabled or when the collector endpoint is empty,
// ensuring global OTEL providers are safe no-ops that do not leak goroutines.
func newNoopTelemetry(cfg TelemetryConfig) (*Telemetry, error) {
	mp := sdkmetric.NewMeterProvider()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(RedactingAttrBagSpanProcessor{Redactor: cfg.Redactor}))
	lp := sdklog.NewLoggerProvider()

	metricsFactory, err := metrics.NewMetricsFactory(mp.Meter(cfg.LibraryName), cfg.Logger)
	if err != nil {
		return nil, err
	}

	return &Telemetry{
		TelemetryConfig: cfg,
		TracerProvider:  tp,
		MeterProvider:   mp,
		LoggerProvider:  lp,
		MetricsFactory:  metricsFactory,
		shutdown:        func() {},
		shutdownCtx:     func(context.Context) error { return nil },
	}, nil
}

// shutdownAll performs best-effort shutdown of all allocated components.
// Used during NewTelemetry to roll back partial allocations on failure.
func shutdownAll(ctx context.Context, components []shutdownable) {
	for _, c := range components {
		if isNilShutdownable(c) {
			continue
		}

		_ = c.Shutdown(ctx)
	}
}

// ApplyGlobals sets this instance as the process-global OTEL providers/propagator.
// Returns an error if any required provider is nil.
func (tl *Telemetry) ApplyGlobals() error {
	if tl == nil {
		return ErrNilTelemetry
	}

	if tl.TracerProvider == nil || tl.MeterProvider == nil || tl.Propagator == nil {
		return ErrNilProvider
	}

	otel.SetTracerProvider(tl.TracerProvider)
	otel.SetMeterProvider(tl.MeterProvider)

	if tl.LoggerProvider != nil {
		global.SetLoggerProvider(tl.LoggerProvider)
	}

	otel.SetTextMapPropagator(tl.Propagator)

	return nil
}

// Tracer returns a tracer from this telemetry instance.
func (tl *Telemetry) Tracer(name string) (trace.Tracer, error) {
	if tl == nil || tl.TracerProvider == nil {
		// Logger is intentionally nil: nil/incomplete Telemetry means no reliable logger available.
		asserter := assert.New(context.Background(), nil, "tracing", "Tracer")
		_ = asserter.NoError(context.Background(), ErrNilTelemetry, "telemetry tracer provider is nil")

		return nil, ErrNilTelemetry
	}

	return tl.TracerProvider.Tracer(name), nil
}

// Meter returns a meter from this telemetry instance.
func (tl *Telemetry) Meter(name string) (metric.Meter, error) {
	if tl == nil || tl.MeterProvider == nil {
		// Logger is intentionally nil: nil/incomplete Telemetry means no reliable logger available.
		asserter := assert.New(context.Background(), nil, "tracing", "Meter")
		_ = asserter.NoError(context.Background(), ErrNilTelemetry, "telemetry meter provider is nil")

		return nil, ErrNilTelemetry
	}

	return tl.MeterProvider.Meter(name), nil
}

// ShutdownTelemetry shuts down telemetry components using background context.
func (tl *Telemetry) ShutdownTelemetry() {
	if tl == nil {
		return
	}

	if err := tl.ShutdownTelemetryWithContext(context.Background()); err != nil {
		asserter := assert.New(context.Background(), tl.Logger, "tracing", "ShutdownTelemetry")
		_ = asserter.NoError(context.Background(), err, "telemetry shutdown failed")

		return
	}
}

// ShutdownTelemetryWithContext shuts down telemetry components with caller context.
func (tl *Telemetry) ShutdownTelemetryWithContext(ctx context.Context) error {
	if tl == nil {
		// Logger is intentionally nil: nil receiver means no Telemetry instance to extract logger from.
		asserter := assert.New(context.Background(), nil, "tracing", "ShutdownTelemetryWithContext")
		_ = asserter.NoError(context.Background(), ErrNilTelemetry, "cannot shutdown nil telemetry")

		return ErrNilTelemetry
	}

	if tl.shutdownCtx != nil {
		return tl.shutdownCtx(ctx)
	}

	if tl.shutdown != nil {
		tl.shutdown()
		return nil
	}

	asserter := assert.New(context.Background(), tl.Logger, "tracing", "ShutdownTelemetryWithContext")
	_ = asserter.NoError(context.Background(), ErrNilShutdown, "cannot shutdown telemetry without configured shutdown function")

	return ErrNilShutdown
}

// ForceFlush flushes whatever the configured providers still hold in memory
// WITHOUT shutting them down, so a short-lived job, a CLI run, or a burst worth
// investigating becomes visible in the collector immediately instead of waiting
// for the next batch interval. Shutdown remains the only thing that closes the
// providers; this may be called any number of times while the process runs.
//
// It flushes the TracerProvider, the MeterProvider, and the LoggerProvider when
// each is present, and joins their errors so one failing signal does not hide
// the others. Nil-safe: a nil receiver returns nil, and so does the noop
// telemetry built when telemetry is disabled or the collector endpoint is empty.
func (tl *Telemetry) ForceFlush(ctx context.Context) error {
	if tl == nil {
		return nil
	}

	var errs []error

	if tl.TracerProvider != nil {
		errs = append(errs, tl.TracerProvider.ForceFlush(ctx))
	}

	if tl.MeterProvider != nil {
		errs = append(errs, tl.MeterProvider.ForceFlush(ctx))
	}

	if tl.LoggerProvider != nil {
		errs = append(errs, tl.LoggerProvider.ForceFlush(ctx))
	}

	return errors.Join(errs...)
}

// newResource builds the resource shared by every provider. The SDK applies
// the options below in order and, on a key conflict, the later one wins
// (resource.Merge: "the value from b will overwrite the value from a"). So the
// generated instance id goes first, the environment detector
// (OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME) second, and the explicit
// config last: explicit config > environment > generated id.
//
// A malformed OTEL_RESOURCE_ATTRIBUTES makes the SDK return
// ErrPartialResource together with the pairs it could parse. That is not
// worth refusing telemetry over: the partial resource is kept and one warning
// is logged. Any other error is returned.
func (tl *TelemetryConfig) newResource(ctx context.Context) (*sdkresource.Resource, error) {
	explicit := []attribute.KeyValue{
		semconv.TelemetrySDKName(constant.TelemetrySDKName),
		semconv.TelemetrySDKLanguageGo,
	}

	// A non-empty config field wins over the environment. An empty one is
	// unset: it falls through to OTEL_SERVICE_NAME / OTEL_RESOURCE_ATTRIBUTES,
	// and when neither supplies a value the attribute is omitted rather than
	// published as an empty string that would overwrite the environment's.
	if strings.TrimSpace(tl.ServiceName) != "" {
		explicit = append(explicit, semconv.ServiceName(tl.ServiceName))
	}

	if strings.TrimSpace(tl.ServiceVersion) != "" {
		explicit = append(explicit, semconv.ServiceVersion(tl.ServiceVersion))
	}

	if strings.TrimSpace(tl.DeploymentEnv) != "" {
		explicit = append(explicit, semconv.DeploymentEnvironmentName(tl.DeploymentEnv))
	}

	if revision := strings.TrimSpace(tl.ServiceRevision); revision != "" {
		explicit = append(explicit, semconv.VCSRefHeadRevision(revision))
	}

	opts := []sdkresource.Option{sdkresource.WithSchemaURL(semconv.SchemaURL)}

	if id := strings.TrimSpace(tl.ServiceInstanceID); id != "" {
		explicit = append(explicit, semconv.ServiceInstanceID(id))
	} else {
		opts = append(opts, sdkresource.WithAttributes(semconv.ServiceInstanceID(uuid.NewString())))
	}

	opts = append(opts, sdkresource.WithFromEnv(), sdkresource.WithAttributes(explicit...))

	r, err := sdkresource.New(ctx, opts...)
	if err != nil {
		if !errors.Is(err, sdkresource.ErrPartialResource) {
			return nil, err
		}

		if !log.IsNil(tl.Logger) {
			tl.Logger.Log(ctx, log.LevelWarn,
				"Malformed OTEL_RESOURCE_ATTRIBUTES; keeping the attributes that parsed",
				log.String("env_var", "OTEL_RESOURCE_ATTRIBUTES"), log.Err(err))
		}
	}

	return r, nil
}

// exporterTLSCredentials builds the transport credentials used by every OTLP
// exporter when InsecureExporter is false. Passing them explicitly is what makes
// the caller's choice win: otlpconfig applies the OTEL_EXPORTER_OTLP_* env vars
// first and the constructor options after, so without an explicit option a
// scheme-less (or http://) endpoint in the environment would downgrade the
// connection to plaintext. A nil *tls.Config would use the SDK default; the
// floor here is TLS 1.2.
func exporterTLSCredentials() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}

func (tl *TelemetryConfig) newLoggerExporter(ctx context.Context) (*otlploggrpc.Exporter, error) {
	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(tl.CollectorExporterEndpoint)}
	if tl.InsecureExporter {
		opts = append(opts, otlploggrpc.WithInsecure())
	} else {
		opts = append(opts, otlploggrpc.WithTLSCredentials(exporterTLSCredentials()))
	}

	return otlploggrpc.New(ctx, opts...)
}

func (tl *TelemetryConfig) newMetricExporter(ctx context.Context) (*otlpmetricgrpc.Exporter, error) {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(tl.CollectorExporterEndpoint)}
	if tl.InsecureExporter {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	} else {
		opts = append(opts, otlpmetricgrpc.WithTLSCredentials(exporterTLSCredentials()))
	}

	return otlpmetricgrpc.New(ctx, opts...)
}

func (tl *TelemetryConfig) newTracerExporter(ctx context.Context) (*otlptrace.Exporter, error) {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(tl.CollectorExporterEndpoint)}
	if tl.InsecureExporter {
		opts = append(opts, otlptracegrpc.WithInsecure())
	} else {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(exporterTLSCredentials()))
	}

	return otlptracegrpc.New(ctx, opts...)
}

func (tl *TelemetryConfig) newLoggerProvider(rsc *sdkresource.Resource, exp sdklog.Exporter) *sdklog.LoggerProvider {
	bp := sdklog.NewBatchProcessor(exp)
	return sdklog.NewLoggerProvider(sdklog.WithResource(rsc), sdklog.WithProcessor(bp))
}

func (tl *TelemetryConfig) newMeterProvider(
	res *sdkresource.Resource,
	exp sdkmetric.Exporter,
	cardinalityLimit int,
) *sdkmetric.MeterProvider {
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	}

	// Zero keeps the SDK default (2000). Past the limit the SDK collapses
	// excess attribute sets into otel.metric.overflow=true and drops tenant
	// identity.
	if cardinalityLimit > 0 {
		opts = append(opts, sdkmetric.WithCardinalityLimit(cardinalityLimit))
	}

	return sdkmetric.NewMeterProvider(opts...)
}

// sampler returns the head sampler for the real TracerProvider, or nil when
// SampleRatio is unset (0) and the SDK default ParentBased(AlwaysSample) must
// be preserved. The ratio is validated by validateSampleRatio before this runs.
func (tl *TelemetryConfig) sampler() sdktrace.Sampler {
	if tl.SampleRatio <= 0 {
		return nil
	}

	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(tl.SampleRatio))
}

func (tl *TelemetryConfig) newTracerProvider(rsc *sdkresource.Resource, exp sdktrace.SpanExporter) *sdktrace.TracerProvider {
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(rsc),
		sdktrace.WithSpanProcessor(RedactingAttrBagSpanProcessor{Redactor: tl.Redactor}),
		sdktrace.WithBatcher(exp),
	}

	if s := tl.sampler(); s != nil {
		opts = append(opts, sdktrace.WithSampler(s))
	}

	return sdktrace.NewTracerProvider(opts...)
}

type shutdownable interface {
	Shutdown(ctx context.Context) error
}

// isNilShutdownable checks for both untyped nil and interface-wrapped typed nil
// (e.g., a concrete pointer that is nil but stored in a shutdownable interface).
func isNilShutdownable(s shutdownable) bool {
	if s == nil {
		return true
	}

	v := reflect.ValueOf(s)

	return v.Kind() == reflect.Pointer && v.IsNil()
}

func buildShutdownHandlers(l log.Universal, components ...shutdownable) (func(), func(context.Context) error) {
	// Normalize once at entry rather than re-checking log.IsNil(l) on every
	// component in the loop below.
	if log.IsNil(l) {
		l = log.NewNop()
	}

	shutdown := func() {
		ctx := context.Background()

		for _, c := range components {
			if isNilShutdownable(c) {
				continue
			}

			if err := c.Shutdown(ctx); err != nil {
				l.Log(ctx, log.LevelError, "telemetry shutdown error", log.Err(err))
			}
		}
	}

	shutdownCtx := func(ctx context.Context) error {
		var errs []error

		for _, c := range components {
			if isNilShutdownable(c) {
				continue
			}

			if err := c.Shutdown(ctx); err != nil {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	}

	return shutdown, shutdownCtx
}

// isNilSpan checks for both untyped nil and interface-wrapped typed nil values.
// trace.Span is an interface, so a concrete pointer that is nil but stored in
// a trace.Span variable would pass a simple `span == nil` check.
func isNilSpan(span trace.Span) bool {
	if span == nil {
		return true
	}

	v := reflect.ValueOf(span)

	return v.Kind() == reflect.Pointer && v.IsNil()
}

// maxSpanErrorLength is the maximum length for error messages written to span status/events.
const maxSpanErrorLength = 1024

// credentialSchemePattern matches a Bearer or Basic scheme word and the credential after it.
var credentialSchemePattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+\S+`)

// sanitizeSpanMessage sanitizes an error message for span output:
// - Redacts the credential after every Bearer or Basic scheme word, keeping the word
// - Truncates to a safe maximum length
func sanitizeSpanMessage(msg string) string {
	msg = credentialSchemePattern.ReplaceAllString(msg, "${1} [REDACTED]")

	if len(msg) > maxSpanErrorLength {
		msg = msg[:maxSpanErrorLength]
		// Ensure valid UTF-8 after truncation
		if !utf8.ValidString(msg) {
			msg = strings.ToValidUTF8(msg, "")
		}
	}

	return msg
}

// HandleSpanBusinessErrorEvent records a business-error event on a span.
func HandleSpanBusinessErrorEvent(span trace.Span, eventName string, err error) {
	if isNilSpan(span) || log.IsNil(err) {
		return
	}

	span.AddEvent(eventName, trace.WithAttributes(attribute.String("error", ErrorMessage(err))))
}

// HandleSpanEvent records a generic event with optional attributes on a span.
func HandleSpanEvent(span trace.Span, eventName string, attributes ...attribute.KeyValue) {
	if isNilSpan(span) {
		return
	}

	span.AddEvent(eventName, trace.WithAttributes(attributes...))
}

// HandleSpanError marks a span as failed and records the error.
func HandleSpanError(span trace.Span, message string, err error) {
	if isNilSpan(span) || log.IsNil(err) {
		return
	}

	// Build status message: avoid malformed ": <err>" when message is empty
	statusMsg := ErrorMessage(err)
	if message != "" {
		statusMsg = sanitizeSpanMessage(message + ": " + statusMsg)
	}

	span.SetStatus(codes.Error, statusMsg)
	span.RecordError(errors.New(statusMsg))
}

// ErrorMessage returns a version of err's message that is always safe to
// record on a span or log line: log.SafeErrorMessage recovers from a panic
// in err.Error() itself (a valid, non-nil error can still panic when
// stringified - see its doc comment for why, errors.Join with a typed-nil
// member is the canonical example), then sanitizeSpanMessage applies the
// same Bearer/Basic-token redaction and length cap every span error field
// already carries. Exported so a caller outside this package that needs an
// error rendered identically to how a span renders it - the HTTP access log
// field in particular - doesn't duplicate the redaction/cap rules.
func ErrorMessage(err error) string {
	return sanitizeSpanMessage(log.SafeErrorMessage(err))
}

// SetSpanAttributesFromValue flattens a value and sets resulting attributes on a span.
func SetSpanAttributesFromValue(span trace.Span, prefix string, value any, r *Redactor) error {
	if isNilSpan(span) {
		return nil
	}

	attrs, err := BuildAttributesFromValue(prefix, value, r)
	if err != nil {
		return err
	}

	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}

	return nil
}

// BuildAttributesFromValue flattens a value into OTEL attributes with optional redaction.
func BuildAttributesFromValue(prefix string, value any, r *Redactor) ([]attribute.KeyValue, error) {
	if value == nil {
		return nil, nil
	}

	processed := value

	if r != nil {
		var err error

		processed, err = ObfuscateStruct(value, r)
		if err != nil {
			return nil, err
		}
	}

	b, err := json.Marshal(processed)
	if err != nil {
		return nil, err
	}

	// Use json.NewDecoder with UseNumber() to preserve numeric precision.
	// This avoids float64 rounding for large integers (e.g., financial amounts).
	var decoded any

	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()

	if err := dec.Decode(&decoded); err != nil {
		return nil, err
	}

	// Use fallback prefix for top-level scalars/slices to avoid empty keys.
	effectivePrefix := sanitizeUTF8String(prefix)
	if effectivePrefix == "" {
		switch decoded.(type) {
		case map[string]any:
			// Maps expand their own keys; empty prefix is fine.
		case []any:
			effectivePrefix = "item"
		default:
			effectivePrefix = defaultAttrPrefix
		}
	}

	attrs := make([]attribute.KeyValue, 0, 16)
	flattenAttributes(&attrs, effectivePrefix, decoded, 0)

	return attrs, nil
}

func flattenAttributes(attrs *[]attribute.KeyValue, prefix string, value any, depth int) {
	if depth >= maxAttributeDepth {
		return
	}

	if len(*attrs) >= maxAttributeCount {
		return
	}

	switch v := value.(type) {
	case map[string]any:
		flattenMap(attrs, prefix, v, depth)
	case []any:
		flattenSlice(attrs, prefix, v, depth)
	case string:
		s := truncateUTF8(sanitizeUTF8String(v), maxSpanAttributeStringLength)
		*attrs = append(*attrs, attribute.String(resolveKey(prefix, defaultAttrPrefix), s))
	case float64:
		*attrs = append(*attrs, attribute.Float64(resolveKey(prefix, defaultAttrPrefix), v))
	case bool:
		*attrs = append(*attrs, attribute.Bool(resolveKey(prefix, defaultAttrPrefix), v))
	case json.Number:
		flattenJSONNumber(attrs, prefix, v)
	case nil:
		return
	default:
		*attrs = append(*attrs, attribute.String(resolveKey(prefix, defaultAttrPrefix), sanitizeUTF8String(fmt.Sprint(v))))
	}
}

// resolveKey returns prefix if non-empty, otherwise falls back to fallback.
func resolveKey(prefix, fallback string) string {
	if prefix == "" {
		return fallback
	}

	return prefix
}

func flattenMap(attrs *[]attribute.KeyValue, prefix string, m map[string]any, depth int) {
	for key, child := range m {
		next := sanitizeUTF8String(key)
		if prefix != "" {
			next = prefix + "." + next
		}

		flattenAttributes(attrs, next, child, depth+1)
	}
}

func flattenSlice(attrs *[]attribute.KeyValue, prefix string, s []any, depth int) {
	idxKey := resolveKey(prefix, "item")
	for i, child := range s {
		next := idxKey + "." + strconv.Itoa(i)
		flattenAttributes(attrs, next, child, depth+1)
	}
}

func flattenJSONNumber(attrs *[]attribute.KeyValue, prefix string, v json.Number) {
	key := resolveKey(prefix, defaultAttrPrefix)

	// Try Int64 first for precision, fall back to Float64
	if i, err := v.Int64(); err == nil {
		*attrs = append(*attrs, attribute.Int64(key, i))
	} else if f, err := v.Float64(); err == nil {
		*attrs = append(*attrs, attribute.Float64(key, f))
	} else {
		*attrs = append(*attrs, attribute.String(key, string(v)))
	}
}

// truncateUTF8 truncates a string to at most maxBytes, ensuring the result is valid UTF-8.
// If the byte-slice cut lands in the middle of a multi-byte rune, incomplete trailing bytes
// are trimmed so the result is always valid.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}

	s = s[:maxBytes]

	// If the truncation produced invalid UTF-8, trim the trailing incomplete rune
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}

	return s
}

// InjectTraceContext injects trace context into a generic text map carrier.
func InjectTraceContext(ctx context.Context, carrier propagation.TextMapCarrier) {
	if carrier == nil {
		return
	}

	otel.GetTextMapPropagator().Inject(ctx, carrier)
}

// ExtractTraceContext extracts trace context from a generic text map carrier.
//
// This is the single funnel every inbound-extraction path in this library
// goes through - directly for HTTP (middleware.ExtractHTTPContext), and
// transitively for gRPC (ExtractGRPCContext) and queue consumers
// (ExtractQueueTraceContext, ExtractTraceContextFromQueueHeaders) - so the
// tenant.id strip below applies uniformly regardless of transport, with no
// per-transport copy to fall out of sync.
func ExtractTraceContext(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	if carrier == nil {
		return ctx
	}

	// Captured BEFORE extraction: propagation.Baggage.Extract does not merge
	// into the existing baggage, it REPLACES the whole value on ctx with
	// whatever it parses from the carrier (or leaves ctx untouched if the
	// carrier had no baggage header at all - see its own source). So an
	// inbound baggage header that carries other members but no tenant.id -
	// not just one that forges a tenant.id - silently wipes an
	// already-seeded, legitimate tenant.id (the one seeded from a validated
	// JWT claim before this middleware ever runs).
	// The whole Member is captured (not just its decoded Value) so the
	// restore below can re-apply it verbatim via SetMember: rebuilding it
	// with baggage.NewMember would treat the DECODED value as
	// percent-encoded input, so a legitimate tenant.id containing a
	// character like ";" would fail validation and silently drop the
	// trusted value.
	preExistingTenantID := baggage.FromContext(ctx).Member(constant.AttrKeyTenantID)

	extracted := stripTenantIDBaggage(otel.GetTextMapPropagator().Extract(ctx, carrier))

	if preExistingTenantID.Value() != "" {
		extracted = restoreTenantIDBaggage(extracted, preExistingTenantID)
	}

	return extracted
}

// restoreTenantIDBaggage re-applies a tenant.id member captured from ctx
// BEFORE extraction, merging it into whatever baggage extraction (plus the
// strip above) left behind - SetMember adds/replaces one member without
// touching the others, unlike Extract's own full replacement. This is what
// makes an already in-process tenant.id always win over anything an inbound
// carrier claims, whether the carrier stayed silent on tenant.id (wiped by
// Extract's replacement) or tried to forge one (removed by the strip): the
// third rail is that tenant identity never comes from the carrier, so the
// trusted, pre-extraction value is the only one that may survive here.
//
// The original Member is re-applied as-is - never rebuilt through
// baggage.NewMember, whose percent-encoding validation would reject (and
// so drop) a decoded value containing characters like ";". A member that
// came out of a parsed Baggage is already valid, so SetMember failing is
// unexpected; when it does, the failure is logged rather than swallowed,
// since it means a trusted tenant.id was lost.
func restoreTenantIDBaggage(ctx context.Context, member baggage.Member) context.Context {
	bag, err := baggage.FromContext(ctx).SetMember(member)
	if err != nil {
		observability.NewLoggerFromContext(ctx).Log(ctx, log.LevelWarn,
			"failed to restore trusted tenant.id baggage member after inbound extraction",
			log.Err(err))

		return ctx
	}

	return baggage.ContextWithBaggage(ctx, bag)
}

// stripTenantIDBaggage removes the tenant.id member from the OTel BAGGAGE
// just extracted from an inbound carrier (HTTP headers, gRPC metadata, or
// queue headers) - this is narrower than "tenant identity never comes from a
// caller-controlled field": it covers the baggage propagation path
// specifically. The composite propagator configured in NewTelemetry extracts
// standard W3C baggage alongside trace context, so an external caller
// sending `baggage: tenant.id=acme-corp` (over any transport) would
// otherwise forge the tenant.id stamped on every span by
// AttrBagSpanProcessor - before auth ever runs. Called unconditionally by
// ExtractTraceContext (a no-op when there is no baggage to strip - see
// bag.Len() below), with no enable/disable knob: unlike inbound
// trace-context trust (see TelemetryConfig.TrustInboundTraceContext), there
// is no deployment topology where trusting a caller-supplied tenant.id is
// correct. Trace-context propagation itself (and any other baggage member)
// is left untouched. ExtractTraceContext restores a pre-existing, trusted
// tenant.id after this runs - see restoreTenantIDBaggage.
//
// This does NOT close every tenant-from-caller-controlled-field surface:
// grpcmiddleware.ResolveTenantIDFromGRPC is a separate, pre-existing
// mechanism that reads a caller-controlled `tenant-id` gRPC metadata field
// directly (not via baggage) for span/metric labeling. See its own doc
// comment - closing that gap is a distinct, not-yet-made decision.
func stripTenantIDBaggage(ctx context.Context) context.Context {
	bag := baggage.FromContext(ctx)
	if bag.Len() == 0 {
		return ctx
	}

	return baggage.ContextWithBaggage(ctx, bag.DeleteMember(constant.AttrKeyTenantID))
}

// InjectHTTPContext injects trace headers into HTTP headers.
func InjectHTTPContext(ctx context.Context, headers http.Header) {
	if headers == nil {
		return
	}

	InjectTraceContext(ctx, propagation.HeaderCarrier(headers))
}

// grpcMetadataHeaderPairs lists the (lowercase gRPC metadata key, canonical
// PascalCase header key) pairs that must be remapped in both directions when
// crossing between metadata.MD (grpc-go always lowercases metadata keys -
// mandated by HTTP/2, which requires lowercase header field names on the
// wire - so no real gRPC client can send anything else) and
// propagation.HeaderCarrier, whose Get/Set canonicalize via textproto (e.g.
// "traceparent" -> "Traceparent"). Without this remapping the canonicalizing
// Get looks up a key that is never present, so the affected field silently
// fails to extract - reproduced for baggage specifically: propagation.Baggage
// found nothing in gRPC metadata, so a gRPC-propagated tenant.id (or any
// other baggage member) never reached a span at all until this fix.
var grpcMetadataHeaderPairs = [...][2]string{
	{constant.MetadataTraceparent, constant.HeaderTraceparentPascal},
	{constant.MetadataTracestate, constant.HeaderTracestatePascal},
	{constant.MetadataBaggage, constant.HeaderBaggagePascal},
}

// InjectGRPCContext injects trace context into gRPC metadata.
func InjectGRPCContext(ctx context.Context, md metadata.MD) metadata.MD {
	if md == nil {
		md = metadata.New(nil)
	}

	InjectTraceContext(ctx, propagation.HeaderCarrier(md))

	for _, pair := range grpcMetadataHeaderPairs {
		lower, pascal := pair[0], pair[1]
		if values, exists := md[pascal]; exists && len(values) > 0 {
			md[lower] = values
			delete(md, pascal)
		}
	}

	return md
}

// ExtractGRPCContext extracts trace context from gRPC metadata.
func ExtractGRPCContext(ctx context.Context, md metadata.MD) context.Context {
	if md == nil {
		return ctx
	}

	mdCopy := md.Copy()

	for _, pair := range grpcMetadataHeaderPairs {
		lower, pascal := pair[0], pair[1]
		if values, exists := mdCopy[lower]; exists && len(values) > 0 {
			mdCopy[pascal] = values
			delete(mdCopy, lower)
		}
	}

	return ExtractTraceContext(ctx, propagation.HeaderCarrier(mdCopy))
}

// InjectQueueTraceContext serializes trace context to string headers for queues.
func InjectQueueTraceContext(ctx context.Context) map[string]string {
	carrier := propagation.HeaderCarrier{}
	InjectTraceContext(ctx, carrier)

	headers := make(map[string]string, len(carrier))
	for k, v := range carrier {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	return headers
}

// ExtractQueueTraceContext extracts trace context from queue string headers.
func ExtractQueueTraceContext(ctx context.Context, headers map[string]string) context.Context {
	if headers == nil {
		return ctx
	}

	carrier := propagation.HeaderCarrier{}
	for k, v := range headers {
		carrier.Set(k, v)
	}

	return ExtractTraceContext(ctx, carrier)
}

// PrepareQueueHeaders merges base headers with propagated trace headers.
func PrepareQueueHeaders(ctx context.Context, baseHeaders map[string]any) map[string]any {
	headers := make(map[string]any)
	maps.Copy(headers, baseHeaders)

	traceHeaders := InjectQueueTraceContext(ctx)
	for k, v := range traceHeaders {
		headers[k] = v
	}

	return headers
}

// InjectTraceHeadersIntoQueue injects propagated trace headers into a mutable map.
func InjectTraceHeadersIntoQueue(ctx context.Context, headers *map[string]any) {
	if headers == nil {
		return
	}

	if *headers == nil {
		*headers = make(map[string]any)
	}

	traceHeaders := InjectQueueTraceContext(ctx)
	for k, v := range traceHeaders {
		(*headers)[k] = v
	}
}

// ExtractTraceContextFromQueueHeaders extracts trace context from AMQP-style headers.
func ExtractTraceContextFromQueueHeaders(baseCtx context.Context, amqpHeaders map[string]any) context.Context {
	if len(amqpHeaders) == 0 {
		return baseCtx
	}

	traceHeaders := make(map[string]string)

	for k, v := range amqpHeaders {
		if str, ok := v.(string); ok {
			traceHeaders[k] = str
		}
	}

	if len(traceHeaders) == 0 {
		return baseCtx
	}

	return ExtractQueueTraceContext(baseCtx, traceHeaders)
}

// GetTraceIDFromContext returns the current span trace ID, or empty if unavailable.
func GetTraceIDFromContext(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)

	sc := span.SpanContext()
	if !sc.IsValid() {
		return ""
	}

	return sc.TraceID().String()
}

// GetTraceStateFromContext returns the current span tracestate, or empty if unavailable.
func GetTraceStateFromContext(ctx context.Context) string {
	span := trace.SpanFromContext(ctx)

	sc := span.SpanContext()
	if !sc.IsValid() {
		return ""
	}

	return sc.TraceState().String()
}

func sanitizeUTF8String(s string) string {
	if !utf8.ValidString(s) {
		return strings.ToValidUTF8(s, "")
	}

	return s
}

// isStrictEnvironment returns true for production-like environments
// where insecure OTEL exporters must not be used.
func isStrictEnvironment(env string) bool {
	switch env {
	case "production", "prod":
		return true
	default:
		return false
	}
}
