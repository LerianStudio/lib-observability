//go:build unit

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	observability "github.com/LerianStudio/lib-observability"
	constant "github.com/LerianStudio/lib-observability/constants"
	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/metadata"
)

func TestResolveTenantIDFromHTTP(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		want   string
	}{
		{name: "canonical header present", header: map[string]string{"X-Tenant-Id": "acme"}, want: "acme"},
		{name: "header absent", header: nil, want: ""},
		{name: "header empty after trim", header: map[string]string{"X-Tenant-Id": "   "}, want: ""},
		{name: "header literal null is dropped", header: map[string]string{"X-Tenant-Id": "null"}, want: ""},
		{name: "header at max length is kept", header: map[string]string{"X-Tenant-Id": strings.Repeat("a", constant.MaxTenantIDLen)}, want: strings.Repeat("a", constant.MaxTenantIDLen)},
		{name: "header above max length is dropped", header: map[string]string{"X-Tenant-Id": strings.Repeat("a", constant.MaxTenantIDLen+1)}, want: ""},
		{name: "control chars are stripped", header: map[string]string{"X-Tenant-Id": "acme\r\n"}, want: "acme"},
		{name: "non-canonical aliases are ignored", header: map[string]string{"tenant-id": "acme"}, want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()

			var got string
			app.Get("/", func(c *fiber.Ctx) error {
				got = ResolveTenantIDFromHTTP(c)
				return nil
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}

			_, err := app.Test(req)
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestResolveTenantIDFromHTTPNilCtx(t *testing.T) {
	assert.Equal(t, "", ResolveTenantIDFromHTTP(nil))
}

func TestResolveTenantIDFromGRPC(t *testing.T) {
	cases := []struct {
		name string
		md   metadata.MD
		want string
	}{
		{name: "canonical metadata present", md: metadata.Pairs("tenant-id", "acme"), want: "acme"},
		{name: "metadata absent", md: nil, want: ""},
		{name: "metadata empty", md: metadata.Pairs("tenant-id", "   "), want: ""},
		{name: "metadata above max length is dropped", md: metadata.Pairs("tenant-id", strings.Repeat("a", constant.MaxTenantIDLen+1)), want: ""},
		{name: "non-canonical aliases are ignored", md: metadata.Pairs("tenant_id", "acme"), want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.md != nil {
				ctx = metadata.NewIncomingContext(ctx, tc.md)
			}

			assert.Equal(t, tc.want, ResolveTenantIDFromGRPC(ctx))
		})
	}
}

func TestResolveTenantIDFromGRPCNilCtx(t *testing.T) {
	assert.Equal(t, "", ResolveTenantIDFromGRPC(nil))
}

func TestTenantIDFromAttrBag_OverrideWins(t *testing.T) {
	// Simulates middleware writing the header tenant, then a downstream auth
	// layer overriding with the JWT-validated tenant. Last-wins must surface
	// the override, not the original.
	ctx := observability.ContextWithSpanAttributes(context.Background(),
		attribute.String(constant.AttrKeyTenantID, "from-header"),
	)
	ctx = observability.ContextWithSpanAttributes(ctx,
		attribute.String(constant.AttrKeyTenantID, "from-jwt"),
	)

	assert.Equal(t, "from-jwt", tenantIDFromAttrBag(ctx))
}

func TestTenantIDFromAttrBag_AbsentReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", tenantIDFromAttrBag(context.Background()))
}

func TestRequestAttributes_ReturnsCopy(t *testing.T) {
	ctx := observability.ContextWithSpanAttributes(context.Background(),
		attribute.String(constant.AttrKeyTenantID, "acme"),
		attribute.String("app.request.request_id", "req-1"),
	)

	attrs := RequestAttributes(ctx)
	assert.Len(t, attrs, 2)

	// Mutating the returned slice must not affect the bag in ctx.
	attrs[0] = attribute.String("tampered", "x")
	again := RequestAttributes(ctx)
	assert.Equal(t, attribute.Key(constant.AttrKeyTenantID), again[0].Key)
}
