//go:build unit

package tracing

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newSpanRecorder(t *testing.T) (trace.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("test"), sr
}

func TestStartClientSpan_DefaultsToClientKind(t *testing.T) {
	tracer, sr := newSpanRecorder(t)

	_, span := StartClientSpan(context.Background(), tracer, "mongodb.find")
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, trace.SpanKindClient, ended[0].SpanKind(),
		"outbound span must default to CLIENT")
	assert.Equal(t, "mongodb.find", ended[0].Name())
}

func TestStartClientSpan_CallerCanOverrideKind(t *testing.T) {
	tracer, sr := newSpanRecorder(t)

	// Caller explicitly asks for SERVER — must win over the CLIENT default
	// because options are last-wins and the helper PREPENDS its default (ADR-004).
	_, span := StartClientSpan(context.Background(), tracer, "custom",
		trace.WithSpanKind(trace.SpanKindServer))
	span.End()

	ended := sr.Ended()
	require.Len(t, ended, 1)
	assert.Equal(t, trace.SpanKindServer, ended[0].SpanKind(),
		"explicit caller kind must override the CLIENT default")
}

func TestStartClientSpan_PassesThroughCallerOptions(t *testing.T) {
	tracer, sr := newSpanRecorder(t)

	_, span := StartClientSpan(context.Background(), tracer, "op",
		trace.WithAttributes())
	span.End()

	require.Len(t, sr.Ended(), 1)
	assert.Equal(t, trace.SpanKindClient, sr.Ended()[0].SpanKind())
}
