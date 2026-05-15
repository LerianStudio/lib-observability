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
