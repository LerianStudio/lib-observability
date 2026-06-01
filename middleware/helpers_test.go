//go:build unit

package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
)

func TestIsRouteExcludedFromList(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		excluded []string
		want     bool
	}{
		// exact match
		{name: "exact match", path: "/health", excluded: []string{"/health"}, want: true},

		// subpath match (segment boundary)
		{name: "subpath under excluded route", path: "/health/check", excluded: []string{"/health"}, want: true},
		{name: "deep subpath", path: "/api/v1/users/42", excluded: []string{"/api/v1"}, want: true},

		// regression guards against the original raw-prefix bug
		{name: "sibling with shared prefix (suffix letter)", path: "/healthz", excluded: []string{"/health"}, want: false},
		{name: "sibling with shared prefix (hyphenated)", path: "/health-check", excluded: []string{"/health"}, want: false},
		{name: "sibling with shared prefix (under /metrics)", path: "/metricsproxy", excluded: []string{"/metrics"}, want: false},
		{name: "minor version not a child", path: "/api/v1.0/users", excluded: []string{"/api/v1"}, want: false},

		// trailing slash tolerance on the excluded entry
		{name: "excluded route with trailing slash matches path without", path: "/metrics", excluded: []string{"/metrics/"}, want: true},
		{name: "excluded route with trailing slash matches subpath", path: "/metrics/cpu", excluded: []string{"/metrics/"}, want: true},

		// empty / root entries are not wildcards
		{name: "empty string entry is not a wildcard", path: "/anything", excluded: []string{""}, want: false},
		{name: "root slash entry is not a wildcard", path: "/anything", excluded: []string{"/"}, want: false},

		// list semantics
		{name: "no excluded routes", path: "/anywhere", excluded: nil, want: false},
		{name: "no match in list", path: "/v1/orders", excluded: []string{"/health", "/readyz"}, want: false},
		{name: "match second entry", path: "/readyz", excluded: []string{"/health", "/readyz"}, want: true},
		{name: "match wins over later non-matches", path: "/health/x", excluded: []string{"/health", "/no-match"}, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			var got bool
			app.Use(func(c *fiber.Ctx) error {
				got = isRouteExcludedFromList(c, tc.excluded)
				return c.SendStatus(http.StatusNoContent)
			})

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			resp, err := app.Test(req)
			assert.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, tc.want, got, "isRouteExcludedFromList(path=%q, excluded=%v)", tc.path, tc.excluded)
		})
	}
}
