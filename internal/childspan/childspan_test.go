//go:build unit

package childspan

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// newProvider builds a real SDK TracerProvider that exports synchronously to an
// in-memory exporter, so a test sees exactly which spans were recorded.
func newProvider(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	return tp, exp
}

// TestRequireParent_NoParentStartsNoSpan is the rule itself: a call with no
// parent span (a background loop, a health check) records nothing and leaves
// the context untouched.
func TestRequireParent_NoParentStartsNoSpan(t *testing.T) {
	tp, exp := newProvider(t)

	ctx := context.Background()

	gotCtx, span := RequireParent(tp).Tracer("test").Start(ctx, "db.query")
	span.End()

	assert.Equal(t, ctx, gotCtx, "context must be returned unchanged")
	assert.False(t, span.IsRecording(), "span must be non-recording")
	assert.False(t, span.SpanContext().IsValid(), "span must carry no span context")
	assert.Empty(t, exp.GetSpans(), "no span may be exported without a parent")
}

// TestRequireParent_LocalParentStartsChild verifies a call inside an existing
// trace is traced normally, as a child of the current span.
func TestRequireParent_LocalParentStartsChild(t *testing.T) {
	tp, exp := newProvider(t)

	ctx, parent := tp.Tracer("test").Start(context.Background(), "request")

	_, child := RequireParent(tp).Tracer("test").Start(ctx, "db.query")
	child.End()
	parent.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 2)

	assert.Equal(t, "db.query", spans[0].Name)
	assert.Equal(t, parent.SpanContext().SpanID(), spans[0].Parent.SpanID())
	assert.Equal(t, parent.SpanContext().TraceID(), spans[0].SpanContext.TraceID())
}

// TestRequireParent_RemoteParentStartsChild verifies a parent extracted from
// incoming headers (a remote span context, no local span) counts as a parent.
func TestRequireParent_RemoteParentStartsChild(t *testing.T) {
	tp, exp := newProvider(t)

	remote := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		SpanID:     trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), remote)

	_, span := RequireParent(tp).Tracer("test").Start(ctx, "db.query")
	span.End()

	spans := exp.GetSpans()
	require.Len(t, spans, 1)

	assert.Equal(t, remote.SpanID(), spans[0].Parent.SpanID())
	assert.Equal(t, remote.TraceID(), spans[0].SpanContext.TraceID())
}
