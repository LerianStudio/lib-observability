package middleware

import (
	"context"
	"encoding/hex"
	"reflect"

	observability "github.com/LerianStudio/lib-observability/v4"
	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// canonicalTenantID renders a tenant id in the platform canonical spelling:
// the 32-character lowercase dashless hex form.
//
// A uuid.UUID is a [16]byte and carries no spelling of its own; the hyphens
// exist only in the rendering. uuid.UUID.String() renders the RFC 4122 dashed
// form, but every other channel a tenant id travels through on this platform
// uses the dashless form: the JWT tenantId claim, the OTel baggage member
// written by lib-commons tmmiddleware.WithTenantDB, the Postgres and Mongo
// connection pool keys, the tenant cache keys and the Redis key namespaces —
// all of them the output of lib-commons core.CanonicalTenantID, which parses
// the UUID and re-encodes it with encoding/hex for exactly this reason.
//
// Rendering a metric or span label with String() therefore splits one tenant
// into two time series that no query can reconcile: dashless on the spans
// seeded from baggage, dashed on the metrics labelled from the attested
// identity. This helper is what keeps the two spellings identical. It applies
// to the tenant id only — contextID, exceptionID and disputeID are ordinary
// UUIDs with no dashless convention and keep their String() rendering.
//
// It is duplicated rather than imported from lib-commons because lib-commons
// depends on lib-observability, and the reverse edge would deadlock the two
// release trains. It takes a uuid.UUID rather than a string, so unlike
// core.CanonicalTenantID it needs no validation and no error path.
func canonicalTenantID(tenantID uuid.UUID) string {
	return hex.EncodeToString(tenantID[:])
}

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
// Why both sinks: setting attributes only on the current span leaves the
// request AttrBag empty, so later application spans and explicitly opted-in
// business metrics cannot reuse the authenticated identity. The built-in HTTP
// server span and duration metric deliberately exclude tenant identity.
//
// Callers MUST use the returned context for downstream work (handler chain,
// c.SetUserContext) so the propagated attributes are visible; the AttrBag lives
// in an immutable context value and cannot be mutated in place.
func SetHandlerSpanAttributes(ctx context.Context, span trace.Span, tenantID, contextID uuid.UUID) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	attrs := []attribute.KeyValue{
		attribute.String(constant.AttrKeyTenantID, canonicalTenantID(tenantID)),
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

	span.SetAttributes(attribute.String(constant.AttrKeyTenantID, canonicalTenantID(tenantID)))
}

// SetExceptionSpanAttributes adds tenant.id and exception.id attributes to a trace span.
func SetExceptionSpanAttributes(span trace.Span, tenantID, exceptionID uuid.UUID) {
	if isNilSpan(span) {
		return
	}

	span.SetAttributes(
		attribute.String(constant.AttrKeyTenantID, canonicalTenantID(tenantID)),
		attribute.String("exception.id", exceptionID.String()),
	)
}

// SetDisputeSpanAttributes adds tenant.id and dispute.id attributes to a trace span.
func SetDisputeSpanAttributes(span trace.Span, tenantID, disputeID uuid.UUID) {
	if isNilSpan(span) {
		return
	}

	span.SetAttributes(
		attribute.String(constant.AttrKeyTenantID, canonicalTenantID(tenantID)),
		attribute.String("dispute.id", disputeID.String()),
	)
}
