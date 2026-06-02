package middleware

import (
	"context"

	observability "github.com/LerianStudio/lib-observability"
	constant "github.com/LerianStudio/lib-observability/constants"
	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel/attribute"
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
// "" when absent. Because ContextWithSpanAttributes deduplicates by key
// (last-wins), this never returns a stale value when a later layer overrode
// the tenant — the bag carries a single entry per key.
func tenantIDFromAttrBag(ctx context.Context) string {
	for _, attr := range observability.AttributesFromContext(ctx) {
		if attr.Key == attribute.Key(constant.AttrKeyTenantID) {
			return attr.Value.AsString()
		}
	}

	return ""
}
