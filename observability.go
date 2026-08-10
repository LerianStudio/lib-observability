// Package observability provides context carriers for attaching and extracting
// OpenTelemetry tracers, metric factories, loggers, and request-scoped span
// attributes. NewTrackingFromContext is the primary convenience entry-point
// used by service handlers and repository methods to obtain all telemetry
// components in a single call.
package observability

import (
	"context"
	"strings"
	"sync"

	"github.com/LerianStudio/lib-observability/v3/log"
	"github.com/LerianStudio/lib-observability/v3/metrics"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ---- Context key ----

type contextKey string

// ContextKey is the context key used to store ContextValue.
var ContextKey = contextKey("custom_context")

// ContextValue holds all request-scoped facilities we attach to context.
type ContextValue struct {
	HeaderID      string
	Tracer        trace.Tracer
	Logger        log.Logger
	MetricFactory *metrics.MetricsFactory

	// AttrBag holds request-wide attributes to be applied to every span.
	// Keep low/medium cardinality attributes here (tenant.id, plan, region, request_id, route).
	AttrBag []attribute.KeyValue
}

// ---- Logger helpers ----

// NewLoggerFromContext extracts the Logger from context.
// A nil ctx is normalized to context.Background() so callers never trigger a nil-pointer dereference.
//
//nolint:ireturn
func NewLoggerFromContext(ctx context.Context) log.Logger {
	if ctx == nil {
		ctx = context.Background()
	}

	if cv, ok := ctx.Value(ContextKey).(*ContextValue); ok && cv != nil {
		return resolveLogger(cv.Logger)
	}

	return log.NewNop()
}

// cloneContextValues returns a shallow copy of the ContextValue from ctx.
// This prevents concurrent mutation of a shared struct when multiple goroutines
// derive child contexts from the same parent.
// The AttrBag slice is deep-copied to avoid aliasing the underlying array.
func cloneContextValues(ctx context.Context) *ContextValue {
	existing, _ := ctx.Value(ContextKey).(*ContextValue)

	clone := &ContextValue{}
	if existing != nil {
		*clone = *existing

		// Deep-copy the slice to avoid aliasing the backing array.
		if len(existing.AttrBag) > 0 {
			clone.AttrBag = make([]attribute.KeyValue, len(existing.AttrBag))
			copy(clone.AttrBag, existing.AttrBag)
		}
	}

	return clone
}

// ContextWithLogger returns a context with the given Logger attached.
func ContextWithLogger(ctx context.Context, logger log.Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	values := cloneContextValues(ctx)
	values.Logger = resolveLogger(logger)

	return context.WithValue(ctx, ContextKey, values)
}

// ---- Tracer helpers ----

// ContextWithTracer returns a context with the given trace.Tracer attached.
func ContextWithTracer(ctx context.Context, tracer trace.Tracer) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	values := cloneContextValues(ctx)
	values.Tracer = tracer

	return context.WithValue(ctx, ContextKey, values)
}

// ---- Metrics helpers ----

// ContextWithMetricFactory returns a context with the given MetricsFactory attached.
func ContextWithMetricFactory(ctx context.Context, metricFactory *metrics.MetricsFactory) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	values := cloneContextValues(ctx)
	values.MetricFactory = metricFactory

	return context.WithValue(ctx, ContextKey, values)
}

// ---- Tracking bundle (convenience) ----

// TrackingComponents represents the complete set of tracking components extracted from context.
// This struct encapsulates all telemetry-related dependencies in a single, cohesive unit.
type TrackingComponents struct {
	Logger        log.Logger
	Tracer        trace.Tracer
	HeaderID      string
	MetricFactory *metrics.MetricsFactory
}

// NewTrackingFromContext extracts tracking components from context with intelligent fallback.
// It follows the fail-safe principle: preserve valid components, provide sensible defaults for invalid ones.
//
//nolint:ireturn
func NewTrackingFromContext(ctx context.Context) (log.Logger, trace.Tracer, string, *metrics.MetricsFactory) {
	if ctx == nil {
		ctx = context.Background()
	}

	components := extractTrackingComponents(ctx)

	return components.Logger, components.Tracer, components.HeaderID, components.MetricFactory
}

// extractTrackingComponents performs the core extraction logic with comprehensive fallback strategy.
func extractTrackingComponents(ctx context.Context) TrackingComponents {
	cv, ok := ctx.Value(ContextKey).(*ContextValue)
	if !ok || cv == nil {
		return newDefaultTrackingComponents()
	}

	return TrackingComponents{
		Logger:        resolveLogger(cv.Logger),
		Tracer:        resolveTracer(cv.Tracer),
		HeaderID:      resolveHeaderID(cv.HeaderID),
		MetricFactory: resolveMetricFactory(cv.MetricFactory),
	}
}

// resolveLogger applies the Null Object Pattern for logger resolution.
// Returns a functional logger instance in all cases, eliminating nil checks downstream.
func resolveLogger(logger log.Logger) log.Logger {
	if !log.IsNil(logger) {
		return logger
	}

	return log.NewNop() // Null Object Pattern - always functional
}

// resolveTracer ensures a valid tracer is always available using OpenTelemetry best practices.
// The default tracer maintains observability even when context is incomplete.
func resolveTracer(tracer trace.Tracer) trace.Tracer {
	if tracer != nil {
		return tracer
	}

	return otel.Tracer("observability.default") // Descriptive tracer name for debugging
}

// resolveHeaderID implements the correlation ID pattern with UUID fallback.
// Ensures every request has a unique identifier for distributed tracing.
//
// IMPORTANT: When no HeaderID is present in context, a new UUID is generated on
// every call to NewTrackingFromContext. Ingress middleware (HTTP/gRPC) MUST persist
// the generated ID back into context via ContextWithSpanAttributes so that downstream
// extractions within the same request return a stable correlation ID.
func resolveHeaderID(headerID string) string {
	if trimmed := strings.TrimSpace(headerID); trimmed != "" {
		return trimmed
	}

	return uuid.New().String() // Generate unique correlation ID
}

var (
	defaultFactoryOnce sync.Once
	defaultFactory     *metrics.MetricsFactory
)

func getDefaultMetricsFactory() *metrics.MetricsFactory {
	defaultFactoryOnce.Do(func() {
		meter := otel.GetMeterProvider().Meter("observability.default")

		f, err := metrics.NewMetricsFactory(meter, log.NewNop())
		if err != nil {
			defaultFactory = metrics.NewNopFactory()
			return
		}

		defaultFactory = f
	})

	return defaultFactory
}

// resolveMetricFactory ensures a valid metrics factory is always available following the fail-safe pattern.
// Provides a cached default factory when none exists, initialized once via sync.Once.
// Never returns nil: if factory creation fails, it falls back to a no-op factory.
func resolveMetricFactory(factory *metrics.MetricsFactory) *metrics.MetricsFactory {
	if factory != nil {
		return factory
	}

	return getDefaultMetricsFactory()
}

// newDefaultTrackingComponents creates a complete set of default components.
// Used when context extraction fails entirely - ensures system remains operational.
func newDefaultTrackingComponents() TrackingComponents {
	return TrackingComponents{
		Logger:        log.NewNop(),
		Tracer:        otel.Tracer("observability.default"),
		HeaderID:      uuid.New().String(),
		MetricFactory: resolveMetricFactory(nil),
	}
}

// ---- Attribute Bag (request-wide span attributes) ----

// ContextWithSpanAttributes merges one or more attributes into the request's
// AttrBag using last-wins semantics: if a key is already present, its value is
// replaced in place; otherwise the attribute is appended. Call this at the
// ingress (HTTP/gRPC middleware) for shared identifiers and again from a
// downstream layer (e.g. after authentication resolves the real tenant) to
// override the value without producing duplicates in the bag.
// Example keys: tenant.id, enduser.id, request.route, region, plan.
func ContextWithSpanAttributes(ctx context.Context, kv ...attribute.KeyValue) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	if len(kv) == 0 {
		return ctx
	}

	values := cloneContextValues(ctx)
	values.AttrBag = mergeAttrBagLastWins(values.AttrBag, kv)

	return context.WithValue(ctx, ContextKey, values)
}

// mergeAttrBagLastWins returns a slice where every key from incoming overrides
// the matching key in bag (in place, preserving the original position), and
// keys not yet present are appended. The bag input is treated as already
// independent (cloneContextValues deep-copies it before calling), so it is
// safe to mutate.
func mergeAttrBagLastWins(bag, incoming []attribute.KeyValue) []attribute.KeyValue {
	for _, attr := range incoming {
		replaced := false

		for i := range bag {
			if bag[i].Key == attr.Key {
				bag[i] = attr
				replaced = true

				break
			}
		}

		if !replaced {
			bag = append(bag, attr)
		}
	}

	return bag
}

// AttributesFromContext returns a shallow copy of the AttrBag slice, safe to reuse by processors.
func AttributesFromContext(ctx context.Context) []attribute.KeyValue {
	if ctx == nil {
		return nil
	}

	if values, ok := ctx.Value(ContextKey).(*ContextValue); ok && values != nil && len(values.AttrBag) > 0 {
		out := make([]attribute.KeyValue, len(values.AttrBag))
		copy(out, values.AttrBag)

		return out
	}

	return nil
}

// ReplaceAttributes resets the current AttrBag with a new set of request-wide span attributes.
func ReplaceAttributes(ctx context.Context, kv ...attribute.KeyValue) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	values := cloneContextValues(ctx)

	values.AttrBag = append([]attribute.KeyValue(nil), kv...)

	return context.WithValue(ctx, ContextKey, values)
}
