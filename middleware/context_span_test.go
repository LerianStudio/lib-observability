//go:build unit

package middleware

import (
	"context"
	"testing"

	observability "github.com/LerianStudio/lib-observability"
	constant "github.com/LerianStudio/lib-observability/constants"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// attrBagValue returns the value stored in the request AttrBag for key, or ""
// when absent. It reads through the public AttributesFromContext accessor so
// the test exercises the same path the span processor and metric labeling use.
func attrBagValue(ctx context.Context, key string) (string, bool) {
	for _, attr := range observability.AttributesFromContext(ctx) {
		if attr.Key == attribute.Key(key) {
			return attr.Value.AsString(), true
		}
	}

	return "", false
}

// spanAttrValue returns the value of key on the recorded span, or "" when absent.
func spanAttrValue(span sdktrace.ReadOnlySpan, key string) (string, bool) {
	for _, attr := range span.Attributes() {
		if attr.Key == attribute.Key(key) {
			return attr.Value.AsString(), true
		}
	}

	return "", false
}

// TestSetHandlerSpanAttributes_PropagatesTenantToAttrBag is the regression
// guard for the root cause: the helper must push tenant.id into the AttrBag so
// WithTelemetry can read it back (tenantIDFromAttrBag) when labeling
// http.server.request.duration and seeding the root span.
func TestSetHandlerSpanAttributes_PropagatesTenantToAttrBag(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	_, span := tp.Tracer("test").Start(context.Background(), "handler-span")

	tenantID := uuid.New()
	contextID := uuid.New()

	ctx := SetHandlerSpanAttributes(context.Background(), span, tenantID, contextID)
	span.End()

	// AttrBag carries tenant.id (the previously missing propagation).
	got, ok := attrBagValue(ctx, constant.AttrKeyTenantID)
	assert.True(t, ok, "tenant.id must be present in the AttrBag")
	assert.Equal(t, tenantID.String(), got)

	// tenantIDFromAttrBag (the exact path WithTelemetry uses) resolves it.
	assert.Equal(t, tenantID.String(), tenantIDFromAttrBag(ctx))

	// context.id is also propagated.
	gotCtxID, ok := attrBagValue(ctx, constant.AttrKeyContextID)
	assert.True(t, ok, "context.id must be present in the AttrBag")
	assert.Equal(t, contextID.String(), gotCtxID)

	// The span still receives both attributes (behavior preserved).
	recorded := recorder.Ended()[0]
	gotSpanTenant, ok := spanAttrValue(recorded, constant.AttrKeyTenantID)
	assert.True(t, ok)
	assert.Equal(t, tenantID.String(), gotSpanTenant)

	gotSpanCtxID, ok := spanAttrValue(recorded, constant.AttrKeyContextID)
	assert.True(t, ok)
	assert.Equal(t, contextID.String(), gotSpanCtxID)
}

// TestSetHandlerSpanAttributes_NilContextIDOmitsContextID confirms a Nil
// contextID is not propagated to either sink, matching the prior span-only
// behavior of skipping the empty UUID.
func TestSetHandlerSpanAttributes_NilContextIDOmitsContextID(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	_, span := tp.Tracer("test").Start(context.Background(), "handler-span")

	tenantID := uuid.New()

	ctx := SetHandlerSpanAttributes(context.Background(), span, tenantID, uuid.Nil)
	span.End()

	assert.Equal(t, tenantID.String(), tenantIDFromAttrBag(ctx))

	_, ok := attrBagValue(ctx, constant.AttrKeyContextID)
	assert.False(t, ok, "context.id must be omitted when contextID is uuid.Nil")

	_, ok = spanAttrValue(recorder.Ended()[0], constant.AttrKeyContextID)
	assert.False(t, ok, "context.id must be omitted on the span when contextID is uuid.Nil")
}

// TestSetHandlerSpanAttributes_NilSpanStillPropagates ensures a nil/typed-nil
// span does not panic and the AttrBag propagation still happens, so the metric
// path keeps the tenant.id even when no recording span is active.
func TestSetHandlerSpanAttributes_NilSpanStillPropagates(t *testing.T) {
	tenantID := uuid.New()

	ctx := SetHandlerSpanAttributes(context.Background(), nil, tenantID, uuid.Nil)

	assert.Equal(t, tenantID.String(), tenantIDFromAttrBag(ctx))
}

// TestSetHandlerSpanAttributes_NilContextDefaults guards against a nil context
// input: the helper must fall back to a background context rather than panic.
func TestSetHandlerSpanAttributes_NilContextDefaults(t *testing.T) {
	tenantID := uuid.New()

	ctx := SetHandlerSpanAttributes(nil, nil, tenantID, uuid.Nil) //nolint:staticcheck // intentional nil ctx

	assert.NotNil(t, ctx)
	assert.Equal(t, tenantID.String(), tenantIDFromAttrBag(ctx))
}
