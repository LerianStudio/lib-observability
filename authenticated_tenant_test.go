//go:build unit

package observability

import (
	"context"
	"testing"

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

func TestAuthenticatedTenantContextNilIdentityClearsInheritedIdentity(t *testing.T) {
	t.Parallel()

	ctx := ContextWithAuthenticatedTenantID(context.Background(), uuid.New())
	ctx = ContextWithAuthenticatedTenantID(ctx, uuid.Nil)

	_, ok := AuthenticatedTenantIDFromContext(ctx)
	assert.False(t, ok)
}
