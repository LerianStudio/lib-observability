//go:build unit

package observability

import (
	"context"
	"testing"

	"github.com/LerianStudio/lib-observability/log"
	"github.com/LerianStudio/lib-observability/metrics"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

func TestContextTrackingComponents(t *testing.T) {
	t.Parallel()

	logger := log.NewNop()
	tracer := otel.Tracer("test")
	factory := metrics.NewNopFactory()

	ctx := ContextWithLogger(nil, logger)
	ctx = ContextWithTracer(ctx, tracer)
	ctx = ContextWithMetricFactory(ctx, factory)
	ctx = ContextWithHeaderID(ctx, " request-1 ")

	gotLogger, gotTracer, gotHeaderID, gotFactory := NewTrackingFromContext(ctx)
	assert.Same(t, logger, gotLogger)
	assert.Equal(t, tracer, gotTracer)
	assert.Equal(t, "request-1", gotHeaderID)
	assert.Same(t, factory, gotFactory)
	assert.Same(t, logger, NewLoggerFromContext(ctx))
}

func TestContextTrackingDefaults(t *testing.T) {
	t.Parallel()

	logger, tracer, headerID, factory := NewTrackingFromContext(nil)
	require.NotNil(t, logger)
	require.NotNil(t, tracer)
	require.NotEmpty(t, headerID)
	require.NotNil(t, factory)
	assert.NotNil(t, NewLoggerFromContext(context.Background()))
	assert.NotNil(t, resolveMetricFactory(nil))
	assert.NotNil(t, newDefaultTrackingComponents().MetricFactory)
}

func TestContextTrackingFallbacksFromEmptyContextValue(t *testing.T) {
	t.Parallel()

	ctx := context.WithValue(context.Background(), ContextKey, &ContextValue{
		HeaderID: "   ",
	})

	logger, tracer, headerID, factory := NewTrackingFromContext(ctx)
	assert.NotNil(t, logger)
	assert.NotNil(t, tracer)
	assert.NotEmpty(t, headerID)
	assert.NotNil(t, factory)
}

func TestContextAttributesAreCopied(t *testing.T) {
	t.Parallel()

	ctx := ContextWithSpanAttributes(context.Background(), attribute.String("tenant.id", "tenant-1"))
	attrs := AttributesFromContext(ctx)
	require.Len(t, attrs, 1)
	attrs[0] = attribute.String("tenant.id", "mutated")

	assert.Equal(t, "tenant-1", AttributesFromContext(ctx)[0].Value.AsString())

	child := ContextWithSpanAttributes(ctx, attribute.String("region", "br"))
	assert.Len(t, AttributesFromContext(ctx), 1)
	assert.Len(t, AttributesFromContext(child), 2)

	replaced := ReplaceAttributes(child)
	assert.Empty(t, AttributesFromContext(replaced))
	assert.Nil(t, AttributesFromContext(nil))
	assert.Same(t, ctx, ContextWithSpanAttributes(ctx))
}
