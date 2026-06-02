//go:build unit

package observability

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"
)

func TestReplaceAttributes(t *testing.T) {
	ctx := ContextWithSpanAttributes(context.Background(),
		attribute.String("service.name", "old"),
		attribute.String("tenant.id", "tenant-1"),
	)

	ctx = ReplaceAttributes(ctx, attribute.String("service.name", "new"))

	attrs := AttributesFromContext(ctx)
	assert.Equal(t, []attribute.KeyValue{attribute.String("service.name", "new")}, attrs)
}

func TestReplaceAttributesNilContext(t *testing.T) {
	ctx := ReplaceAttributes(nil, attribute.String("service.name", "new"))

	attrs := AttributesFromContext(ctx)
	assert.Equal(t, []attribute.KeyValue{attribute.String("service.name", "new")}, attrs)
}

func TestContextWithSpanAttributes_DedupesByKey(t *testing.T) {
	// Writing the same key twice must collapse to a single entry with the
	// most recent value (last-wins). This is the contract that lets a
	// downstream layer (e.g. JWT auth) override a value provided by the
	// ingress middleware without bloating the bag or producing stale reads.
	ctx := ContextWithSpanAttributes(context.Background(),
		attribute.String("tenant.id", "from-header"),
	)
	ctx = ContextWithSpanAttributes(ctx,
		attribute.String("tenant.id", "from-jwt"),
	)

	attrs := AttributesFromContext(ctx)
	assert.Equal(t, []attribute.KeyValue{attribute.String("tenant.id", "from-jwt")}, attrs)
}

func TestContextWithSpanAttributes_PreservesOrderForNewKeys(t *testing.T) {
	ctx := ContextWithSpanAttributes(context.Background(),
		attribute.String("app.request.request_id", "req-1"),
	)
	ctx = ContextWithSpanAttributes(ctx,
		attribute.String("tenant.id", "acme"),
	)
	ctx = ContextWithSpanAttributes(ctx,
		attribute.String("app.request.request_id", "req-1"), // duplicate, must not append
	)

	attrs := AttributesFromContext(ctx)
	assert.Len(t, attrs, 2)
	assert.Equal(t, attribute.Key("app.request.request_id"), attrs[0].Key)
	assert.Equal(t, attribute.Key("tenant.id"), attrs[1].Key)
}
