package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	observability "github.com/LerianStudio/lib-observability/v2"
	"github.com/LerianStudio/lib-observability/v2/assert"
	constant "github.com/LerianStudio/lib-observability/v2/constants"
	"github.com/LerianStudio/lib-observability/v2/log"
	"github.com/LerianStudio/lib-observability/v2/metrics"
	"github.com/LerianStudio/lib-observability/v2/redaction"
	"github.com/gofiber/fiber/v3"
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
)

// TelemetryConfig configures tracing, metrics, logging, and propagation behavior.
type TelemetryConfig struct {
	LibraryName               string
	ServiceName               string
	ServiceVersion            string
	DeploymentEnv             string
	CollectorExporterEndpoint string
	EnableTelemetry           bool
	InsecureExporter          bool
	// TrustInboundTraceContext controls whether inbound HTTP trace-context
	// extraction (a W3C traceparent/tracestate header) continues that trace,
	// instead of starting a fresh root span for every request. It follows
	// the Go zero-value convention: default false (fail-closed), opt-in
	// true. It replaces the previous internal-service User-Agent heuristic
	// in middleware.WithTelemetry, which was spoofable by any caller that
	// set the header and never a real trust boundary. This flag governs the
	// HTTP path only - the gRPC server interceptor keeps its own,
	// pre-existing User-Agent gate untouched.
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
	// ALWAYS stripped regardless of this flag - the strip lives in the
	// shared ExtractTraceContext funnel every extraction path (HTTP, gRPC,
	// queue) goes through, see its doc comment. This is narrower than
	// "tenant identity never comes from a caller-controlled field" - it
	// covers baggage specifically. middleware.ResolveTenantIDFromGRPC is a
	// separate, pre-existing mechanism that DOES read a caller-controlled
	// `tenant-id` gRPC metadata field for span/metric labeling.
	TrustInboundTraceContext bool
	Logger                   log.Logger
	Propagator               propagation.TextMapPropagator
	Redactor                 *Redactor
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
	if cfg.Logger == nil {
		return nil, ErrNilTelemetryLogger
	}

	if cfg.Propagator == nil {
		cfg.Propagator = propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	}

	if cfg.Redactor == nil {
		cfg.Redactor = NewDefaultRedactor()
	}

	normalizeEndpoint(&cfg)
	normalizeEndpointEnvVars(cfg.Logger)

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

	return initExporters(ctx, cfg)
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
// which fails on bare "host:port" values. Adding "http://" prevents noisy
// "parse url" errors from the SDK's internal logger.
func normalizeEndpointEnvVars(logger log.Logger) {
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
		if err := os.Setenv(key, "http://"+v); err != nil && logger != nil {
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
func initExporters(ctx context.Context, cfg TelemetryConfig) (*Telemetry, error) {
	r := cfg.newResource()

	// Track all allocated resources for rollback if a later step fails.
	var cleanups []shutdownable

	tExp, err := cfg.newTracerExporter(ctx)
	if err != nil {
		return nil, fmt.Errorf("can't initialize tracer exporter: %w", err)
	}

	cleanups = append(cleanups, tExp)

	mExp, err := cfg.newMetricExporter(ctx)
	if err != nil {
		shutdownAll(ctx, cleanups)

		return nil, fmt.Errorf("can't initialize metric exporter: %w", err)
	}

	cleanups = append(cleanups, mExp)

	lExp, err := cfg.newLoggerExporter(ctx)
	if err != nil {
		shutdownAll(ctx, cleanups)

		return nil, fmt.Errorf("can't initialize logger exporter: %w", err)
	}

	cleanups = append(cleanups, lExp)

	mp := cfg.newMeterProvider(r, mExp)
	cleanups = append(cleanups, mp)

	tp := cfg.newTracerProvider(r, tExp)
	cleanups = append(cleanups, tp)

	lp := cfg.newLoggerProvider(r, lExp)
	cleanups = append(cleanups, lp)

	metricsFactory, err := metrics.NewMetricsFactory(mp.Meter(cfg.LibraryName), cfg.Logger)
	if err != nil {
		shutdownAll(ctx, cleanups)

		return nil, err
	}

	shutdown, shutdownCtx := buildShutdownHandlers(cfg.Logger, mp, tp, lp, tExp, mExp, lExp)

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

func (tl *TelemetryConfig) newResource() *sdkresource.Resource {
	return sdkresource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(tl.ServiceName),
		semconv.ServiceVersion(tl.ServiceVersion),
		semconv.DeploymentEnvironmentName(tl.DeploymentEnv),
		semconv.TelemetrySDKName(constant.TelemetrySDKName),
		semconv.TelemetrySDKLanguageGo,
	)
}

func (tl *TelemetryConfig) newLoggerExporter(ctx context.Context) (*otlploggrpc.Exporter, error) {
	opts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(tl.CollectorExporterEndpoint)}
	if tl.InsecureExporter {
		opts = append(opts, otlploggrpc.WithInsecure())
	}

	return otlploggrpc.New(ctx, opts...)
}

func (tl *TelemetryConfig) newMetricExporter(ctx context.Context) (*otlpmetricgrpc.Exporter, error) {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(tl.CollectorExporterEndpoint)}
	if tl.InsecureExporter {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}

	return otlpmetricgrpc.New(ctx, opts...)
}

func (tl *TelemetryConfig) newTracerExporter(ctx context.Context) (*otlptrace.Exporter, error) {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(tl.CollectorExporterEndpoint)}
	if tl.InsecureExporter {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}

	return otlptracegrpc.New(ctx, opts...)
}

func (tl *TelemetryConfig) newLoggerProvider(rsc *sdkresource.Resource, exp *otlploggrpc.Exporter) *sdklog.LoggerProvider {
	bp := sdklog.NewBatchProcessor(exp)
	return sdklog.NewLoggerProvider(sdklog.WithResource(rsc), sdklog.WithProcessor(bp))
}

func (tl *TelemetryConfig) newMeterProvider(res *sdkresource.Resource, exp *otlpmetricgrpc.Exporter) *sdkmetric.MeterProvider {
	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	)
}

func (tl *TelemetryConfig) newTracerProvider(rsc *sdkresource.Resource, exp *otlptrace.Exporter) *sdktrace.TracerProvider {
	return sdktrace.NewTracerProvider(
		sdktrace.WithResource(rsc),
		sdktrace.WithSpanProcessor(RedactingAttrBagSpanProcessor{Redactor: tl.Redactor}),
		sdktrace.WithBatcher(exp),
	)
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

func buildShutdownHandlers(l log.Logger, components ...shutdownable) (func(), func(context.Context) error) {
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

// sanitizeSpanMessage sanitizes an error message for span output:
// - Truncates to a safe maximum length
// - Strips common sensitive-looking patterns (bearer tokens, passwords in URLs)
func sanitizeSpanMessage(msg string) string {
	// Strip common sensitive patterns
	for _, pattern := range []struct{ prefix, replacement string }{
		{"Bearer ", "Bearer [REDACTED]"},
		{"Basic ", "Basic [REDACTED]"},
	} {
		if idx := strings.Index(msg, pattern.prefix); idx >= 0 {
			end := idx + len(pattern.prefix)
			// Find the end of the token (next space or end of string)
			tokenEnd := strings.IndexByte(msg[end:], ' ')
			if tokenEnd < 0 {
				msg = msg[:idx] + pattern.replacement
			} else {
				msg = msg[:idx] + pattern.replacement + msg[end+tokenEnd:]
			}
		}
	}

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

// SetSpanAttributeForParam adds a request parameter attribute to the current context bag.
// Sensitive parameter names (as determined by redaction.IsSensitiveField) are masked.
func SetSpanAttributeForParam(c fiber.Ctx, param, value, entityName string) {
	if c == nil {
		return
	}

	spanAttrKey := "app.request." + param
	if entityName != "" && param == "id" {
		spanAttrKey = "app.request." + entityName + "_id"
	}

	// Mask value if the parameter name is considered sensitive
	attrValue := value
	if redaction.IsSensitiveField(param) {
		attrValue = "[REDACTED]"
	}

	c.SetContext(observability.ContextWithSpanAttributes(c.Context(), attribute.String(spanAttrKey, attrValue)))
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
// goes through - directly for HTTP (ExtractHTTPContext), and transitively
// for gRPC (ExtractGRPCContext) and queue consumers (ExtractQueueTraceContext)
// - so the tenant.id strip below applies uniformly regardless of transport,
// with no per-transport copy to fall out of sync.
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
	preExistingTenantID := baggage.FromContext(ctx).Member(constant.AttrKeyTenantID).Value()

	extracted := stripTenantIDBaggage(otel.GetTextMapPropagator().Extract(ctx, carrier))

	if preExistingTenantID != "" {
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
func restoreTenantIDBaggage(ctx context.Context, tenantID string) context.Context {
	member, err := baggage.NewMember(constant.AttrKeyTenantID, tenantID)
	if err != nil {
		return ctx
	}

	bag, err := baggage.FromContext(ctx).SetMember(member)
	if err != nil {
		return ctx
	}

	return baggage.ContextWithBaggage(ctx, bag)
}

// stripTenantIDBaggage removes the tenant.id member from the OTel BAGGAGE
// just extracted from an inbound carrier (HTTP headers, gRPC metadata, or
// queue headers) - this is narrower than "tenant identity never comes from a
// caller-controlled field": it covers the baggage propagation path
// specifically. An external caller sending `baggage: tenant.id=acme-corp`
// (over any transport) would otherwise forge the tenant.id stamped on every
// span - before auth ever runs. Called unconditionally by
// ExtractTraceContext (a no-op when there is no baggage to strip - see
// bag.Len() below), with no enable/disable knob: unlike inbound
// trace-context trust (see TelemetryConfig.TrustInboundTraceContext), there
// is no deployment topology where trusting a caller-supplied tenant.id is
// correct. Trace-context propagation itself (and any other baggage member)
// is left untouched. ExtractTraceContext restores a pre-existing, trusted
// tenant.id after this runs - see restoreTenantIDBaggage.
//
// This does NOT close every tenant-from-caller-controlled-field surface:
// middleware.ResolveTenantIDFromGRPC is a separate, pre-existing mechanism
// that reads a caller-controlled `tenant-id` gRPC metadata field directly
// (not via baggage) for span/metric labeling.
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

// ExtractHTTPContext extracts trace headers from a Fiber request.
func ExtractHTTPContext(ctx context.Context, c fiber.Ctx) context.Context {
	if c == nil {
		return ctx
	}

	carrier := propagation.HeaderCarrier{}
	for key, value := range c.Request().Header.All() {
		carrier.Set(string(key), string(value))
	}

	return ExtractTraceContext(ctx, carrier)
}

// InjectGRPCContext injects trace context into gRPC metadata.
func InjectGRPCContext(ctx context.Context, md metadata.MD) metadata.MD {
	if md == nil {
		md = metadata.New(nil)
	}

	InjectTraceContext(ctx, propagation.HeaderCarrier(md))

	if traceparentValues, exists := md[constant.HeaderTraceparentPascal]; exists && len(traceparentValues) > 0 {
		md[constant.MetadataTraceparent] = traceparentValues
		delete(md, constant.HeaderTraceparentPascal)
	}

	if tracestateValues, exists := md[constant.HeaderTracestatePascal]; exists && len(tracestateValues) > 0 {
		md[constant.MetadataTracestate] = tracestateValues
		delete(md, constant.HeaderTracestatePascal)
	}

	return md
}

// ExtractGRPCContext extracts trace context from gRPC metadata.
func ExtractGRPCContext(ctx context.Context, md metadata.MD) context.Context {
	if md == nil {
		return ctx
	}

	mdCopy := md.Copy()

	if traceparentValues, exists := mdCopy[constant.MetadataTraceparent]; exists && len(traceparentValues) > 0 {
		mdCopy[constant.HeaderTraceparentPascal] = traceparentValues
		delete(mdCopy, constant.MetadataTraceparent)
	}

	if tracestateValues, exists := mdCopy[constant.MetadataTracestate]; exists && len(tracestateValues) > 0 {
		mdCopy[constant.HeaderTracestatePascal] = tracestateValues
		delete(mdCopy, constant.MetadataTracestate)
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
