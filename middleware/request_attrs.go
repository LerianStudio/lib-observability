package middleware

import (
	"context"

	observability "github.com/LerianStudio/lib-observability/v2"
	"go.opentelemetry.io/otel/attribute"
)

// RequestAttributes returns a copy of the request-scoped attribute bag
// populated by the HTTP/gRPC middleware (tenant.id, app.request.request_id,
// etc.). It is intended for explicit, opt-in inclusion of those attributes
// as metric labels — for example:
//
//	meter.CounterBuilder("orders.created").
//	    WithAttributes(middleware.RequestAttributes(ctx)...).
//	    Add(ctx, 1)
//
// The middleware does NOT add these attributes to metrics automatically:
// metric labels are a high-impact cardinality decision that must remain in
// the hands of the caller. Logs and traces already receive the bag via the
// logging middleware and AttrBagSpanProcessor respectively, so no caller
// action is required there.
func RequestAttributes(ctx context.Context) []attribute.KeyValue {
	return observability.AttributesFromContext(ctx)
}
