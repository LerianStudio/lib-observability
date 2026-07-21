package middleware

import (
	"context"
	"reflect"

	observability "github.com/LerianStudio/lib-observability/v2"
	constant "github.com/LerianStudio/lib-observability/v2/constants"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// isNilSpan reports whether span is nil, including typed-nil interface values
// where a concrete nil pointer is stored in a trace.Span interface.
// This prevents panics when calling methods on a typed-nil span.
func isNilSpan(span trace.Span) bool {
	if span == nil {
		return true
	}

	v := reflect.ValueOf(span)

	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// SetHandlerSpanAttributes adds tenant.id and context.id attributes to the
// current trace span AND propagates them into the request-wide AttrBag carried
// by ctx, returning the enriched context.
//
// Why both sinks: setting attributes only on the child span (the previous
// behavior) left the AttrBag empty, so when WithTelemetry read the tenant.id
// back from the AttrBag after c.Next() to label http.server.request.duration
// (via tenantIDFromAttrBag) it found nothing, and the metric/root span lost the
// tenant.id. Funneling the same attributes through ContextWithSpanAttributes
// fixes that: the AttrBagSpanProcessor copies them onto the root span and the
// duration metric gains the tenant.id label.
//
// Callers MUST use the returned context for downstream work (handler chain,
// c.SetUserContext) so the propagated attributes are visible; the AttrBag lives
// in an immutable context value and cannot be mutated in place.
func SetHandlerSpanAttributes(ctx context.Context, span trace.Span, tenantID, contextID uuid.UUID) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	attrs := []attribute.KeyValue{
		attribute.String(constant.AttrKeyTenantID, tenantID.String()),
	}

	if contextID != uuid.Nil {
		attrs = append(attrs, attribute.String(constant.AttrKeyContextID, contextID.String()))
	}

	if !isNilSpan(span) {
		span.SetAttributes(attrs...)
	}

	return observability.ContextWithSpanAttributes(ctx, attrs...)
}

// SetTenantSpanAttribute adds tenant.id attribute to a trace span.
func SetTenantSpanAttribute(span trace.Span, tenantID uuid.UUID) {
	if isNilSpan(span) {
		return
	}

	span.SetAttributes(attribute.String(constant.AttrKeyTenantID, tenantID.String()))
}

// SetExceptionSpanAttributes adds tenant.id and exception.id attributes to a trace span.
func SetExceptionSpanAttributes(span trace.Span, tenantID, exceptionID uuid.UUID) {
	if isNilSpan(span) {
		return
	}

	span.SetAttributes(
		attribute.String(constant.AttrKeyTenantID, tenantID.String()),
		attribute.String("exception.id", exceptionID.String()),
	)
}

// SetDisputeSpanAttributes adds tenant.id and dispute.id attributes to a trace span.
func SetDisputeSpanAttributes(span trace.Span, tenantID, disputeID uuid.UUID) {
	if isNilSpan(span) {
		return
	}

	span.SetAttributes(
		attribute.String(constant.AttrKeyTenantID, tenantID.String()),
		attribute.String("dispute.id", disputeID.String()),
	)
}
