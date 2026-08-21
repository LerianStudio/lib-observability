package observability

import (
	"context"

	"github.com/google/uuid"
)

type authenticatedTenantContextKey struct{}

// ContextWithAuthenticatedTenantID marks tenantID as identity that an
// application authentication layer has already verified. Shared telemetry
// middleware never promotes request headers, baggage, or transport metadata to
// this trust level automatically.
//
// Call this only after validating the credential and authorizing the tenant.
// A later call replaces an earlier authenticated tenant (last write wins).
// Passing uuid.Nil explicitly clears any authenticated tenant inherited from a
// parent context.
func ContextWithAuthenticatedTenantID(ctx context.Context, tenantID uuid.UUID) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	return context.WithValue(ctx, authenticatedTenantContextKey{}, tenantID)
}

// AuthenticatedTenantIDFromContext returns the tenant identity explicitly
// attested by an application authentication layer. A nil UUID is treated as
// absent. Request headers, baggage, generic span attributes, and gRPC metadata
// are deliberately not consulted.
func AuthenticatedTenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	if ctx == nil {
		return uuid.Nil, false
	}

	tenantID, ok := ctx.Value(authenticatedTenantContextKey{}).(uuid.UUID)
	if !ok || tenantID == uuid.Nil {
		return uuid.Nil, false
	}

	return tenantID, true
}
