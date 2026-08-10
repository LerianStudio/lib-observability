package middleware

import (
	"context"
	"reflect"
	"strings"

	"github.com/gofiber/fiber/v3"
	"google.golang.org/grpc/metadata"
)

const unmatchedRouteTemplate = "/{unmatched}"

const maxRequestIDLength = 128

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
// telemetry attribute, plus a present flag. Fiber exposes Route().Path
// == "/" for unmatched requests (its default catch-all), which would
// conflate early refusals and scanner traffic with the actual root handler.
// Matched is the routing authority independent of the response status.
func routeAttribute(c fiber.Ctx) (string, bool) {
	if c == nil {
		return "", false
	}

	if !c.Matched() {
		return "", false
	}

	r := c.Route()
	if r == nil {
		return "", false
	}

	return r.Path, true
}

// resolvedHTTPRoute returns the matched route template or a stable fallback
// for unmatched traffic. It must only be used after the downstream Fiber
// chain has returned, when Route().Path is reliable.
func resolvedHTTPRoute(c fiber.Ctx) string {
	routePath, present := routeAttribute(c)
	if !present {
		return unmatchedRouteTemplate
	}

	return routePath
}

// maxUserAgentAttrLen caps the user_agent.original span attribute to avoid
// inflating trace storage/index cost. 256 bytes is sufficient for canonical
// client/library/version identifiers in practice.
const maxUserAgentAttrLen = 256

// truncateUserAgent caps the user-agent string at maxUserAgentAttrLen bytes,
// truncating at a rune boundary so the returned string is always valid UTF-8.
// Compliant user-agents are ASCII, but defensive callers may receive
// multi-byte sequences; a byte-level slice could leave a partial rune in the
// span attribute.
func truncateUserAgent(ua string) string {
	if len(ua) <= maxUserAgentAttrLen {
		return ua
	}

	// for i := range ua iterates over rune boundaries; i is the byte index
	// at the start of each rune. We track the last boundary that still fits
	// within the cap and return up to it, so the result never exceeds
	// maxUserAgentAttrLen bytes and never splits a rune.
	lastFit := 0

	for i := range ua {
		if i > maxUserAgentAttrLen {
			return ua[:lastFit]
		}

		lastFit = i
	}

	return ua[:lastFit]
}

// errorTypeOriginal returns the originating Go type name of handlerErr,
// suitable as a high-cardinality debugging attribute on spans. Returns
// "" if handlerErr is nil. Unwraps all pointer levels so "***fiber.Error"
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

// isRouteExcludedFromList reports whether the request path matches any
// excluded route on a path-segment boundary. A route matches when the
// path equals it exactly or starts with "route + /", so "/health" excludes
// "/health" and "/health/check" but NOT "/healthz" or "/health-check".
//
// Trailing slashes on excluded entries are tolerated, and empty entries
// are ignored so they cannot act as accidental wildcards.
func isRouteExcludedFromList(c fiber.Ctx, excludedRoutes []string) bool {
	path := c.Path()

	for _, route := range excludedRoutes {
		route = strings.TrimRight(route, "/")
		if route == "" {
			continue
		}

		if path == route || strings.HasPrefix(path, route+"/") {
			return true
		}
	}

	return false
}

// sanitizeLogValue removes control characters (newlines, carriage returns, null bytes)
// from a string to prevent log injection attacks (CWE-117).
func sanitizeLogValue(raw string) string {
	replacer := strings.NewReplacer("\r", "", "\n", "", "\x00", "")
	return replacer.Replace(raw)
}

// isNilOrEmptyString reports whether a string pointer is nil or the trimmed value is empty.
// "null" and "nil" are treated as empty to handle JSON null serialization artifacts
// where some encoders emit the literal string "null" or "nil" instead of a JSON null.
func isNilOrEmptyString(s *string) bool {
	return s == nil || strings.TrimSpace(*s) == "" || strings.TrimSpace(*s) == "null" || strings.TrimSpace(*s) == "nil"
}

// normalizeRequestID returns a bounded, injection-safe identifier suitable
// for headers and telemetry.
//
// It rejects-and-regenerates rather than rewriting: only control bytes (CR,
// LF, NUL, and the rest of printable ASCII's complement) are stripped,
// because those enable header/log injection (CWE-113/CWE-117). Every other
// printable ASCII character - including ':', '/', '+', '=' - passes through
// unchanged, so an existing cross-service ID format (namespaced identifiers,
// URL-safe base64, ULIDs with a service prefix) survives intact instead of
// being rewritten into a different string that breaks log correlation joins
// between services. Idempotent: running an already-clean ID through this
// function a second time returns the same value, since it has nothing left
// to strip or truncate.
//
// The caller regenerates a UUID when this returns "" (isNilOrEmptyString in
// setRequestHeaderID / getMetadataID) - this function only ever strips or
// truncates, never substitutes.
func normalizeRequestID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	var normalized strings.Builder

	normalized.Grow(min(len(raw), maxRequestIDLength))

	for _, char := range raw {
		if !isSafeRequestIDCharacter(char) {
			continue
		}

		if normalized.Len() >= maxRequestIDLength {
			break
		}

		normalized.WriteRune(char)
	}

	return strings.TrimSpace(normalized.String())
}

// isSafeRequestIDCharacter reports whether a rune is safe to carry in a
// correlation ID: printable ASCII (0x20-0x7E), which by construction
// excludes CR, LF, NUL and every other control byte - the actual injection
// vector - without narrowing the allowed punctuation set.
func isSafeRequestIDCharacter(char rune) bool {
	return char >= 0x20 && char <= 0x7E
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
