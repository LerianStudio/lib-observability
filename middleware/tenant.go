package middleware

import (
	"context"

	observability "github.com/LerianStudio/lib-observability"
	constant "github.com/LerianStudio/lib-observability/constants"
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"google.golang.org/grpc/metadata"
)

// ResolveTenantIDFromHTTP returns the tenant identifier carried by the
// canonical X-Tenant-Id header, normalized for safe inclusion in telemetry.
// Returns an empty string when the header is absent, empty after trimming, or
// longer than MaxTenantIDLen bytes. The header is trusted only as an
// observability hint: callers MUST authenticate the tenant separately and
// override via observability.ContextWithSpanAttributes when the real value
// differs from the header.
func ResolveTenantIDFromHTTP(c *fiber.Ctx) string {
	if c == nil {
		return ""
	}

	return sanitizeTenantID(c.Get(constant.HeaderTenantID))
}

// ResolveTenantIDFromGRPC returns the tenant identifier carried by the
// canonical tenant-id gRPC metadata key, normalized for safe inclusion in
// telemetry. Returns an empty string when the metadata is absent, empty, or
// longer than MaxTenantIDLen bytes. Same trust caveat as the HTTP variant.
func ResolveTenantIDFromGRPC(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || md == nil {
		return ""
	}

	vals := md.Get(constant.MetadataTenantID)
	if len(vals) == 0 {
		return ""
	}

	return sanitizeTenantID(vals[0])
}

// sanitizeTenantID trims whitespace and control bytes from raw, then enforces
// the MaxTenantIDLen cap. Returns "" for any value that fails normalization or
// exceeds the cap, so callers can use it as a presence check.
func sanitizeTenantID(raw string) string {
	value := normalizeRequestID(raw)
	if isNilOrEmptyString(&value) {
		return ""
	}

	if len(value) > constant.MaxTenantIDLen {
		return ""
	}

	return value
}

// tenantIDFromAttrBag returns the tenant.id stored in the request AttrBag, or
// "" when absent or when the stored value would breach the telemetry
// cardinality cap. Because ContextWithSpanAttributes deduplicates by key
// (last-wins), this never returns a stale value when a later layer overrode
// the tenant — the bag carries a single entry per key.
//
// The result is funneled through sanitizeTenantID so the constraints applied
// at ingestion (MaxTenantIDLen cap, control-byte stripping) also hold for
// values injected directly via observability.ContextWithSpanAttributes (for
// example, a handler that wants to override the header-supplied tenant with
// one resolved from a JWT). Without this defense-in-depth step a caller
// could silently bypass the cap and inflate log fields and metric label
// cardinality.
func tenantIDFromAttrBag(ctx context.Context) string {
	for _, attr := range observability.AttributesFromContext(ctx) {
		if attr.Key == attribute.Key(constant.AttrKeyTenantID) {
			return sanitizeTenantID(attr.Value.AsString())
		}
	}

	return ""
}

// tenantIDFromBaggage returns the tenant.id carried by the standard OTel
// baggage (written upstream by lib-commons), normalized through
// sanitizeTenantID so the same cardinality guards as every other ingestion
// path apply. Returns "" when the member is absent.
func tenantIDFromBaggage(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	return sanitizeTenantID(baggage.FromContext(ctx).Member(constant.AttrKeyTenantID).Value())
}

// resolveTenantIDForLogging resolves the tenant.id to attach to a request
// logger using the same base→override precedence as the span processor: the
// standard OTel baggage is the base source, and the request AttrBag
// (header/JWT-resolved) overrides it when present. Returns "" when neither
// source carries a usable value.
func resolveTenantIDForLogging(ctx context.Context) string {
	if tenantID := tenantIDFromAttrBag(ctx); tenantID != "" {
		return tenantID
	}

	return tenantIDFromBaggage(ctx)
}
