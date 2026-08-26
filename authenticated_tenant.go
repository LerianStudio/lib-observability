package observability

import (
	"context"
	"strings"

	constant "github.com/LerianStudio/lib-observability/v3/constants"
	"github.com/google/uuid"
)

type authenticatedTenantContextKey struct{}

// AuthenticatedTenant is the identity attested by the application
// authentication layer. ID is the stable aggregation key. Name is an optional,
// mutable display label and must never replace ID as the aggregation key.
type AuthenticatedTenant struct {
	ID   uuid.UUID
	Name string
}

// ContextWithAuthenticatedTenant marks tenantID as identity already verified by
// the application authentication layer, together with a human-readable name for
// display. Shared telemetry middleware never promotes headers, baggage, or
// transport metadata to this trust level.
//
// Call this only after validating the credential. Passing uuid.Nil clears any
// inherited authenticated tenant. The name is trimmed, length-capped, and
// dropped when empty; it is never required for the metric to be emitted.
func ContextWithAuthenticatedTenant(ctx context.Context, tenantID uuid.UUID, name string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}

	tenant := AuthenticatedTenant{ID: tenantID}
	if tenantID != uuid.Nil {
		tenant.Name = sanitizeAuthenticatedTenantName(name)
	}

	return context.WithValue(ctx, authenticatedTenantContextKey{}, tenant)
}

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
	return ContextWithAuthenticatedTenant(ctx, tenantID, "")
}

// AuthenticatedTenantFromContext returns the attested tenant identity and its
// optional display name. A nil UUID is treated as absent.
func AuthenticatedTenantFromContext(ctx context.Context) (AuthenticatedTenant, bool) {
	if ctx == nil {
		return AuthenticatedTenant{}, false
	}

	tenant, ok := ctx.Value(authenticatedTenantContextKey{}).(AuthenticatedTenant)
	if !ok || tenant.ID == uuid.Nil {
		return AuthenticatedTenant{}, false
	}

	return tenant, true
}

// AuthenticatedTenantIDFromContext returns the tenant identity explicitly
// attested by an application authentication layer. A nil UUID is treated as
// absent. Request headers, baggage, generic span attributes, and gRPC metadata
// are deliberately not consulted.
func AuthenticatedTenantIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	tenant, ok := AuthenticatedTenantFromContext(ctx)
	if !ok {
		return uuid.Nil, false
	}

	return tenant.ID, true
}

func sanitizeAuthenticatedTenantName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	runes := []rune(name)
	if len(runes) > constant.MaxTenantNameLen {
		return string(runes[:constant.MaxTenantNameLen])
	}

	return name
}
