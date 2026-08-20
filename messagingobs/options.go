package messagingobs

import (
	"github.com/LerianStudio/lib-observability/v3/tracing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// defaultLibraryName is the instrumentation scope used for the meter and tracer
// when the caller does not override it with WithLibraryName / WithTelemetry.
const defaultLibraryName = "lib-observability/messagingobs"

// config holds resolved helper options.
type config struct {
	meterProvider  metric.MeterProvider
	tracerProvider trace.TracerProvider
	libraryName    string
}

// Option configures the messaging instrumentation helpers.
type Option func(*config)

// WithMeterProvider sets the MeterProvider for the messaging duration
// histograms. When unset the global provider is used (no-op unless the service
// installed one, e.g. via Telemetry.ApplyGlobals). Nil ignored.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.meterProvider = mp
		}
	}
}

// WithTracerProvider sets the TracerProvider for the PRODUCER / CONSUMER span.
// When unset the global provider is used (no-op unless the service installed
// one). Nil ignored.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		if tp != nil {
			c.tracerProvider = tp
		}
	}
}

// WithLibraryName overrides the instrumentation scope name reported on the
// emitted metrics and spans. Empty ignored.
func WithLibraryName(name string) Option {
	return func(c *config) {
		if name != "" {
			c.libraryName = name
		}
	}
}

// WithTelemetry derives the providers and the instrumentation scope from a
// Telemetry built by this library. It is a convenience for services that
// already hold one and do not install the OTel globals; a nil Telemetry, or one
// with nil providers, leaves the defaults untouched.
//
// Libraries instrumenting on a service's behalf should NOT require this: the
// zero-option form already resolves the globals, which is what makes the
// instrumentation work on a dependency bump alone.
func WithTelemetry(tl *tracing.Telemetry) Option {
	return func(c *config) {
		if tl == nil {
			return
		}

		if tl.MeterProvider != nil {
			c.meterProvider = tl.MeterProvider
		}

		if tl.TracerProvider != nil {
			c.tracerProvider = tl.TracerProvider
		}

		if tl.LibraryName != "" {
			c.libraryName = tl.LibraryName
		}
	}
}

// newConfig resolves options over the global providers. Both globals are no-op
// implementations until the service installs real ones, so the resolved config
// is always safe to instrument against.
func newConfig(opts ...Option) config {
	cfg := config{
		meterProvider:  otel.GetMeterProvider(),
		tracerProvider: otel.GetTracerProvider(),
		libraryName:    defaultLibraryName,
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	return cfg
}

// tracer returns the instrumentation tracer for the resolved scope.
func (c config) tracer() trace.Tracer {
	return c.tracerProvider.Tracer(c.libraryName)
}

// meter returns the instrumentation meter for the resolved scope.
func (c config) meter() metric.Meter {
	return c.meterProvider.Meter(c.libraryName)
}
