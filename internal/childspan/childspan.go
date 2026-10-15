// Package childspan holds the TracerProvider wrapper that stops database and
// cache client spans from starting traces of their own.
//
// Background loops and health checks run SQL and Redis commands outside any
// request. With no parent span, each command becomes its own one-to-three span
// trace, several times per second, even when there is no work to do — trace
// volume that says nothing the duration metric does not already say. sqlobs and
// redisobs wrap their TracerProvider with RequireParent, so a command is traced
// only as part of a trace that already exists.
package childspan

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// RequireParent wraps tp so its tracers start a span only when the context
// already holds a valid span context — a local parent span or a remote one
// extracted from incoming headers. Without one, Start returns the context
// unchanged together with the non-recording span it already carries, so the
// instrumented call runs untraced.
//
// The decision is made per call from the context alone. This is deliberately
// NOT a sampler: an sdktrace.Sampler sees no instrumentation scope, so it would
// also drop legitimate parentless spans from other instrumentation, and
// installing one with WithSampler overrides OTEL_TRACES_SAMPLER.
func RequireParent(tp trace.TracerProvider) trace.TracerProvider {
	return requireParentProvider{delegate: tp}
}

type requireParentProvider struct {
	embedded.TracerProvider

	delegate trace.TracerProvider
}

func (p requireParentProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return requireParentTracer{delegate: p.delegate.Tracer(name, opts...)}
}

type requireParentTracer struct {
	embedded.Tracer

	delegate trace.Tracer
}

func (t requireParentTracer) Start(
	ctx context.Context,
	spanName string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx, trace.SpanFromContext(ctx)
	}

	// The caller ends the span, exactly as with the wrapped tracer's Start;
	// spancheck cannot see that End() from here.
	//nolint:spancheck // span ownership is transferred to the caller
	return t.delegate.Start(ctx, spanName, opts...)
}
