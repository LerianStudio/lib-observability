package middleware

import (
	"context"

	observability "github.com/LerianStudio/lib-observability/v2"
	"github.com/LerianStudio/lib-observability/v2/redaction"
	"github.com/LerianStudio/lib-observability/v2/tracing"
	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
)

// SetSpanAttributeForParam adds a request parameter attribute to the current context bag.
// Sensitive parameter names (as determined by redaction.IsSensitiveField) are masked.
//
// Moved here from the tracing package: it depends on fiber.Ctx, which must not
// leak into the tracing core so Fiber-v2 apps can consume tracing without pulling
// in Fiber v3. Behavior is identical to the previous tracing.SetSpanAttributeForParam.
func SetSpanAttributeForParam(c fiber.Ctx, param, value, entityName string) {
	if c == nil {
		return
	}

	spanAttrKey := "app.request." + param
	if entityName != "" && param == "id" {
		spanAttrKey = "app.request." + entityName + "_id"
	}

	// Mask value if the parameter name is considered sensitive
	attrValue := value
	if redaction.IsSensitiveField(param) {
		attrValue = "[REDACTED]"
	}

	c.SetContext(observability.ContextWithSpanAttributes(c.Context(), attribute.String(spanAttrKey, attrValue)))
}

// ExtractHTTPContext extracts trace headers from a Fiber request.
//
// Moved here from the tracing package: it depends on fiber.Ctx. It delegates
// carrier-level extraction to tracing.ExtractTraceContext, which remains
// fiber-free and is also where the tenant.id baggage member is always
// stripped (that funnel is shared with the gRPC and queue extraction paths -
// see its doc comment), so this function carries no tenant-stripping logic
// of its own.
func ExtractHTTPContext(ctx context.Context, c fiber.Ctx) context.Context {
	if c == nil {
		return ctx
	}

	carrier := propagation.HeaderCarrier{}
	for key, value := range c.Request().Header.All() {
		carrier.Set(string(key), string(value))
	}

	return tracing.ExtractTraceContext(ctx, carrier)
}
