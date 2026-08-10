package middleware

import (
	"context"

	observability "github.com/LerianStudio/lib-observability/v3"
	"go.opentelemetry.io/otel/attribute"
)

// RequestAttributes returns a copy of the request-scoped attribute bag. HTTP
// middleware adds transport correlation attributes such as request ID, while
// authenticated identity must be attached explicitly by the application. The
// result is intended for opt-in inclusion in business metric labels, for example:
//
//	meter.CounterBuilder("orders.created").
//	    WithAttributes(middleware.RequestAttributes(ctx)...).
//	    Add(ctx, 1)
//
// The middleware does NOT add identity attributes to HTTP metrics automatically:
// metric labels are a high-impact cardinality decision that must remain in
// the hands of the caller.
func RequestAttributes(ctx context.Context) []attribute.KeyValue {
	return observability.AttributesFromContext(ctx)
}
