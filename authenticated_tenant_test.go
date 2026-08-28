//go:build unit

package observability

import (
	"context"
	"strings"
	"testing"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticatedTenantContextRoundTrip(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	ctx := ContextWithAuthenticatedTenantID(context.Background(), tenantID)

	got, ok := AuthenticatedTenantIDFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, tenantID, got)
}

func TestAuthenticatedTenantContextRejectsAbsentAndNilIdentity(t *testing.T) {
	t.Parallel()

	_, ok := AuthenticatedTenantIDFromContext(nil)
	assert.False(t, ok)

	ctx := ContextWithAuthenticatedTenantID(nil, uuid.Nil)
	_, ok = AuthenticatedTenantIDFromContext(ctx)
	assert.False(t, ok)
}

func TestAuthenticatedTenantContextLastWriteWins(t *testing.T) {
	t.Parallel()

	first := uuid.New()
	second := uuid.New()
	ctx := ContextWithAuthenticatedTenantID(context.Background(), first)
	ctx = ContextWithAuthenticatedTenantID(ctx, second)

	got, ok := AuthenticatedTenantIDFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, second, got)
}

func TestAuthenticatedTenantContextNilIdentityClearsInheritedIdentity(t *testing.T) {
	t.Parallel()

	ctx := ContextWithAuthenticatedTenantID(context.Background(), uuid.New())
	ctx = ContextWithAuthenticatedTenantID(ctx, uuid.Nil)

	_, ok := AuthenticatedTenantIDFromContext(ctx)
	assert.False(t, ok)
}

func TestAuthenticatedTenantContextRoundTripWithName(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	ctx := ContextWithAuthenticatedTenant(context.Background(), tenantID, "jeff")

	tenant, ok := AuthenticatedTenantFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, AuthenticatedTenant{ID: tenantID, Name: "jeff"}, tenant)

	gotID, ok := AuthenticatedTenantIDFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, tenantID, gotID)
}

func TestAuthenticatedTenantContextSanitizesName(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	oversized := strings.Repeat("ç", constant.MaxTenantNameLen+10)
	ctx := ContextWithAuthenticatedTenant(context.Background(), tenantID, "  "+oversized+"  ")

	tenant, ok := AuthenticatedTenantFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, strings.Repeat("ç", constant.MaxTenantNameLen), tenant.Name)
}

func TestAuthenticatedTenantContextOmitsBlankName(t *testing.T) {
	t.Parallel()

	tenantID := uuid.New()
	ctx := ContextWithAuthenticatedTenant(context.Background(), tenantID, " 	\n ")

	tenant, ok := AuthenticatedTenantFromContext(ctx)
	require.True(t, ok)
	assert.Equal(t, tenantID, tenant.ID)
	assert.Empty(t, tenant.Name)
}

func TestAuthenticatedTenantContextNilIdentityClearsName(t *testing.T) {
	t.Parallel()

	ctx := ContextWithAuthenticatedTenant(context.Background(), uuid.New(), "jeff")
	ctx = ContextWithAuthenticatedTenant(ctx, uuid.Nil, "forged")

	_, ok := AuthenticatedTenantFromContext(ctx)
	assert.False(t, ok)
}
