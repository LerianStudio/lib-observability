package tracing

import (
	"context"
	"strings"

	observability "github.com/LerianStudio/lib-observability/v4"
	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ---- SpanProcessor that applies the AttrBag to every new span ----

// AttrBagSpanProcessor copies request-scoped attributes from context into every span at start.
type AttrBagSpanProcessor struct{}

// RedactingAttrBagSpanProcessor copies request attributes and applies redaction rules by key.
type RedactingAttrBagSpanProcessor struct {
	Redactor *Redactor
}

// OnStart applies request-scoped context attributes to newly started spans.
//
// Sources are applied base-first, override-last so SetAttributes' last-wins
// semantics keep the precedence correct per key:
//  1. standard OTel baggage (e.g. tenant.id propagated by lib-commons) — base;
//  2. the request AttrBag (header/JWT-resolved values) — override.
func (AttrBagSpanProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	if base := baggageBaseAttributes(ctx); len(base) > 0 {
		s.SetAttributes(base...)
	}

	if kv := observability.AttributesFromContext(ctx); len(kv) > 0 {
		s.SetAttributes(kv...)
	}
}

// OnStart applies request-scoped attributes and redacts sensitive values before writing to span.
//
// Like AttrBagSpanProcessor.OnStart it layers the standard OTel baggage as the
// base source and the request AttrBag as the override, redacting each source by
// key before writing so the last-wins precedence survives redaction.
func (p RedactingAttrBagSpanProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	base := baggageBaseAttributes(ctx)
	kv := observability.AttributesFromContext(ctx)

	if len(base) == 0 && len(kv) == 0 {
		return
	}

	if p.Redactor != nil {
		base = redactAttributesByKey(base, p.Redactor)
		kv = redactAttributesByKey(kv, p.Redactor)
	}

	if len(base) > 0 {
		s.SetAttributes(base...)
	}

	if len(kv) > 0 {
		s.SetAttributes(kv...)
	}
}

// baggageBaseAttributes extracts the shared identifiers carried by the standard
// OTel baggage (currently tenant.id, written upstream by lib-commons) so they
// can seed a span before the request AttrBag overrides them. Returns nil when
// no recognized member is present.
func baggageBaseAttributes(ctx context.Context) []attribute.KeyValue {
	if v := sanitizeTenantFromBaggage(baggage.FromContext(ctx).Member(constant.AttrKeyTenantID).Value()); v != "" {
		return []attribute.KeyValue{attribute.String(constant.AttrKeyTenantID, v)}
	}

	return nil
}

// sanitizeTenantFromBaggage applies the same cardinality guards used on the
// logging path (middleware.sanitizeTenantID) to raw baggage values: strip the
// log-injection control bytes (\r \n \x00), trim surrounding whitespace, and
// drop values exceeding MaxTenantIDLen. Mirroring those rules here keeps span
// and log tenant.id identical, preventing cardinality drift between the two.
// It is duplicated rather than imported because middleware depends on tracing,
// so importing middleware here would create a cycle.
func sanitizeTenantFromBaggage(raw string) string {
	replacer := strings.NewReplacer("\r", "", "\n", "", "\x00", "")
	v := strings.TrimSpace(replacer.Replace(raw))

	// Treat literal "null"/"nil" (case-insensitive) as empty, mirroring the
	// middleware path, to handle JSON null serialization artifacts.
	if strings.EqualFold(v, "null") || strings.EqualFold(v, "nil") {
		return ""
	}

	if v == "" || len(v) > constant.MaxTenantIDLen {
		return ""
	}

	return v
}

// OnEnd is a no-op for this processor.
func (AttrBagSpanProcessor) OnEnd(sdktrace.ReadOnlySpan) {}

// OnEnd is a no-op for this processor.
func (RedactingAttrBagSpanProcessor) OnEnd(sdktrace.ReadOnlySpan) {}

// Shutdown is a no-op and always returns nil.
func (AttrBagSpanProcessor) Shutdown(context.Context) error { return nil }

// Shutdown is a no-op and always returns nil.
func (RedactingAttrBagSpanProcessor) Shutdown(context.Context) error { return nil }

// ForceFlush is a no-op and always returns nil.
func (AttrBagSpanProcessor) ForceFlush(context.Context) error { return nil }

// ForceFlush is a no-op and always returns nil.
func (RedactingAttrBagSpanProcessor) ForceFlush(context.Context) error { return nil }

func redactAttributesByKey(attrs []attribute.KeyValue, redactor *Redactor) []attribute.KeyValue {
	if redactor == nil {
		return attrs
	}

	redacted := make([]attribute.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		key := string(attr.Key)

		fieldName := key
		if idx := strings.LastIndex(key, "."); idx >= 0 && idx+1 < len(key) {
			fieldName = key[idx+1:]
		}

		action, ok := redactor.actionFor(key, fieldName)
		if !ok {
			redacted = append(redacted, attr)
			continue
		}

		switch action {
		case RedactionDrop:
			continue
		case RedactionHash:
			redacted = append(redacted, attribute.String(string(attr.Key), redactor.hashString(attr.Value.String())))
		default:
			redacted = append(redacted, attribute.String(string(attr.Key), redactor.maskValue))
		}
	}

	return redacted
}
