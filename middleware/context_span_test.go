//go:build unit

package middleware

import (
	"context"
	"testing"

	observability "github.com/LerianStudio/lib-observability/v4"
	constant "github.com/LerianStudio/lib-observability/v4/constants"
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
	assert.Equal(t, canonicalTenantID(tenantID), got)

	// tenantIDFromAttrBag (the exact path WithTelemetry uses) resolves it.
	assert.Equal(t, canonicalTenantID(tenantID), tenantIDFromAttrBag(ctx))

	// context.id is also propagated.
	gotCtxID, ok := attrBagValue(ctx, constant.AttrKeyContextID)
	assert.True(t, ok, "context.id must be present in the AttrBag")
	assert.Equal(t, contextID.String(), gotCtxID)

	// The span still receives both attributes (behavior preserved).
	recorded := recorder.Ended()[0]
	gotSpanTenant, ok := spanAttrValue(recorded, constant.AttrKeyTenantID)
	assert.True(t, ok)
	assert.Equal(t, canonicalTenantID(tenantID), gotSpanTenant)

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

	assert.Equal(t, canonicalTenantID(tenantID), tenantIDFromAttrBag(ctx))

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

	assert.Equal(t, canonicalTenantID(tenantID), tenantIDFromAttrBag(ctx))
}

// TestSetHandlerSpanAttributes_NilContextDefaults guards against a nil context
// input: the helper must fall back to a background context rather than panic.
func TestSetHandlerSpanAttributes_NilContextDefaults(t *testing.T) {
	tenantID := uuid.New()

	ctx := SetHandlerSpanAttributes(nil, nil, tenantID, uuid.Nil) //nolint:staticcheck // intentional nil ctx

	assert.NotNil(t, ctx)
	assert.Equal(t, canonicalTenantID(tenantID), tenantIDFromAttrBag(ctx))
}

// The platform canonical spelling of a tenant id is the 32-character lowercase
// dashless hex form produced by lib-commons core.CanonicalTenantID. The JWT
// claim, the OTel baggage member and every tenant-keyed pool, cache and Redis
// key carry that form, so a span or metric label rendered as
// uuid.UUID.String() would split one tenant into two series. This pins the
// dashless form against hardcoded literals rather than deriving the
// expectation from the same helper under test.
func TestCanonicalTenantID_RendersDashlessLowercaseHex(t *testing.T) {
	tests := []struct {
		name string
		in   uuid.UUID
		want string
	}{
		{
			name: "lowercase uuid",
			in:   uuid.MustParse("550e8400-e29b-41d4-a716-446655440000"),
			want: "550e8400e29b41d4a716446655440000",
		},
		{
			name: "uppercase input folds to lowercase",
			in:   uuid.MustParse("0F6E2B3A-1C4D-4E5F-8A9B-0C1D2E3F4A5B"),
			want: "0f6e2b3a1c4d4e5f8a9b0c1d2e3f4a5b",
		},
		{
			name: "nil uuid",
			in:   uuid.Nil,
			want: "00000000000000000000000000000000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalTenantID(tt.in)

			assert.Equal(t, tt.want, got)
			assert.Len(t, got, 32)
			assert.NotContains(t, got, "-")
		})
	}
}
