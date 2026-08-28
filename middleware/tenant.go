package middleware

import (
	"context"
	"strings"

	observability "github.com/LerianStudio/lib-observability/v4"
	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"google.golang.org/grpc/metadata"
)

// ResolveTenantIDFromHTTP returns the tenant identifier carried by the
// canonical X-Tenant-Id header, normalized for safe inclusion in telemetry.
// Returns an empty string when the header is absent, empty after trimming, or
// longer than MaxTenantIDLen bytes. This compatibility API does not make the
// shared HTTP logging, tracing, or metrics middleware consume the header.
// Callers MUST authenticate tenant identity before using the returned value in
// explicit application telemetry.
func ResolveTenantIDFromHTTP(c fiber.Ctx) string {
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

// sanitizeTenantID enforces the MaxTenantIDLen cap on the trimmed raw value,
// then trims whitespace and control bytes. The length check MUST run before
// normalizeRequestID: that helper truncates (rather than rejects) input
// longer than its own, separate cap, so checking length afterward would let
// an oversized tenant ID silently survive as a truncated - and therefore
// wrong - value instead of being dropped outright. An oversized tenant
// identifier must never partially resolve, since tenant.id feeds telemetry
// cardinality and (via callers that trust it further) authorization context.
// Returns "" for any value that fails normalization or exceeds the cap, so
// callers can use it as a presence check.
func sanitizeTenantID(raw string) string {
	if len(strings.TrimSpace(raw)) > constant.MaxTenantIDLen {
		return ""
	}

	value := normalizeRequestID(raw)
	if isNilOrEmptyString(&value) {
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
// could silently bypass the cap and inflate telemetry cardinality.
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

// resolveTenantIDForTelemetry resolves the tenant.id used by explicit telemetry
// paths such as the gRPC request logger. The request AttrBag overrides standard
// OTel baggage when both carry a usable value. Built-in HTTP telemetry does not
// call this resolver because infrastructure signals must not carry identity.
func resolveTenantIDForTelemetry(ctx context.Context) string {
	if tenantID := tenantIDFromAttrBag(ctx); tenantID != "" {
		return tenantID
	}

	return tenantIDFromBaggage(ctx)
}
