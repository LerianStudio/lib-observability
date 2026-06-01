package middleware

import (
	"context"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	constant "github.com/LerianStudio/lib-observability/constants"
	"github.com/LerianStudio/lib-observability/redaction"
	"github.com/gofiber/fiber/v2"
	"google.golang.org/grpc/metadata"
)

// uuidPattern matches standard UUID v4 strings (8-4-4-4-12 hex digits).
var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// internalServicePattern matches Lerian internal service user-agent strings.
var internalServicePattern = regexp.MustCompile(`^[\w-]+/[\d.]+\s+LerianStudio$`)

// knownHTTPMethods is the canonical case-sensitive set per OpenTelemetry
// HTTP semantic conventions; methods outside this set are reported as
// "_OTHER" on telemetry to keep label cardinality bounded.
var knownHTTPMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "DELETE": {},
	"CONNECT": {}, "OPTIONS": {}, "TRACE": {}, "PATCH": {},
}

// normalizeHTTPMethod returns the canonical method label and, if a
// substitution happened, the original verb. Comparison is intentionally
// case-sensitive: compliant clients send uppercase, and lowercase variants
// genuinely belong in "_OTHER".
func normalizeHTTPMethod(raw string) (normalized, original string, replaced bool) {
	if _, ok := knownHTTPMethods[raw]; ok {
		return raw, "", false
	}

	return "_OTHER", raw, true
}

// routeAttribute returns the route template suitable for the http.route
// telemetry attribute, plus a present flag. Fiber v2 exposes Route().Path
// == "/" for unmatched requests (its default catch-all), which would
// conflate scanner/404 traffic with the actual root handler in dashboards.
// We detect this case (effective status == 404 AND route == "/" AND the
// request path is NOT "/") and report the attribute as absent so callers
// can omit it entirely, matching OTel guidance that http.route SHOULD be
// absent when no route matched.
func routeAttribute(c *fiber.Ctx, effectiveStatus int) (string, bool) {
	if c == nil {
		return "", false
	}

	r := c.Route()
	if r == nil {
		return "", false
	}

	if effectiveStatus == fiber.StatusNotFound && r.Path == "/" && c.Path() != "/" {
		return "", false
	}

	return r.Path, true
}

// errorTypeOriginal returns the originating Go type name of handlerErr,
// suitable as a high-cardinality debugging attribute on spans. Returns
// "" if handlerErr is nil. Unwraps a single pointer level so "*fiber.Error"
// surfaces as "fiber.Error". Falls back to "error" when reflect cannot
// resolve a meaningful name.
func errorTypeOriginal(handlerErr error) string {
	if handlerErr == nil {
		return ""
	}

	t := reflect.TypeOf(handlerErr)
	if t == nil {
		return "error"
	}

	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if name := t.String(); name != "" {
		return name
	}

	return "error"
}

// isRouteExcludedFromList reports whether the request path matches any excluded route prefix.
// This standalone function is used to evaluate route exclusions independently of whether
// the TelemetryMiddleware receiver is nil.
func isRouteExcludedFromList(c *fiber.Ctx, excludedRoutes []string) bool {
	for _, route := range excludedRoutes {
		if strings.HasPrefix(c.Path(), route) {
			return true
		}
	}

	return false
}

// sanitizeURL removes or obfuscates sensitive query parameters from URLs
// to prevent exposing tokens, API keys, and other sensitive data in telemetry.
func sanitizeURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return sanitizeMalformedURL(rawURL)
	}

	if parsed.RawQuery == "" {
		return rawURL
	}

	query := parsed.Query()
	modified := false

	for key := range query {
		if redaction.IsSensitiveField(key) {
			query.Set(key, constant.ObfuscatedValue)

			modified = true
		}
	}

	if !modified {
		return rawURL
	}

	parsed.RawQuery = query.Encode()

	return parsed.String()
}

func sanitizeMalformedURL(rawURL string) string {
	sanitized := sanitizeLogValue(rawURL)
	if before, _, ok := strings.Cut(sanitized, "?"); ok {
		return before + "?redacted"
	}

	return sanitized
}

// sanitizeLogValue removes control characters (newlines, carriage returns, null bytes)
// from a string to prevent log injection attacks (CWE-117).
func sanitizeLogValue(raw string) string {
	replacer := strings.NewReplacer("\r", "", "\n", "", "\x00", "")
	return replacer.Replace(raw)
}

// getGRPCUserAgent extracts the User-Agent from incoming gRPC metadata.
// Returns empty string if the metadata is not present or doesn't contain user-agent.
func getGRPCUserAgent(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || md == nil {
		return ""
	}

	userAgents := md.Get(strings.ToLower(headerUserAgent))
	if len(userAgents) == 0 {
		return ""
	}

	return userAgents[0]
}

// isInternalLerianService reports whether a user-agent belongs to a Lerian internal service.
func isInternalLerianService(userAgent string) bool {
	return internalServicePattern.MatchString(userAgent)
}

// replaceUUIDWithPlaceholder replaces UUIDs with a placeholder in a given path string.
func replaceUUIDWithPlaceholder(path string) string {
	return uuidPattern.ReplaceAllString(path, ":id")
}

// isNilOrEmptyString reports whether a string pointer is nil or the trimmed value is empty.
// "null" and "nil" are treated as empty to handle JSON null serialization artifacts
// where some encoders emit the literal string "null" or "nil" instead of a JSON null.
func isNilOrEmptyString(s *string) bool {
	return s == nil || strings.TrimSpace(*s) == "" || strings.TrimSpace(*s) == "null" || strings.TrimSpace(*s) == "nil"
}

// normalizeRequestID trims whitespace and control characters from a raw request ID.
func normalizeRequestID(raw string) string {
	return strings.TrimSpace(sanitizeLogValue(raw))
}

// normalizeGRPCContext returns a non-nil context, falling back to context.Background().
func normalizeGRPCContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}

	return ctx
}

// getMetadataID extracts a correlation id from incoming gRPC metadata if present.
func getMetadataID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	if md, ok := metadata.FromIncomingContext(ctx); ok && md != nil {
		headerID := md.Get(metadataID)
		if len(headerID) > 0 {
			v := normalizeRequestID(headerID[0])
			if v != "" && v != "null" && v != "nil" {
				return v
			}
		}
	}

	return ""
}
