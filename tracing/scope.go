package tracing

import (
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceTracer returns a tracer on the service scope, for spans the service's
// own code opens. Without a Telemetry or a TracerProvider it returns a noop
// tracer, never nil, so a caller can always Start a span on it.
func (tl *Telemetry) ServiceTracer() trace.Tracer {
	if tl == nil {
		return noop.NewTracerProvider().Tracer("")
	}

	name := tl.LibraryName

	if tl.TracerProvider == nil {
		return noop.NewTracerProvider().Tracer(name)
	}

	return tl.TracerProvider.Tracer(name)
}
