package tracing

import (
	"context"
	"slices"

	"github.com/LerianStudio/lib-observability/v4/redaction"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// WithDocumentScrubbing wraps the OTLP span exporter so every exported span's
// name, status description, string attributes, event names and attributes
// (exception.message included) and link attributes pass through
// redaction.ScrubDocuments, which replaces each CPF/CNPJ-shaped span with
// redaction.DocumentPlaceholder.
//
// It runs at export, after the span has ended, so it catches text from every
// source alike: HandleSpanError, panic and assertion instrumentation, and a
// caller's own RecordError or SetStatus. A span without documents is exported
// as the original value. Numeric attributes are not inspected. The option is
// ignored when telemetry runs on noop providers, which export nothing.
//
// This is defence in depth, not a licence to format documents into span names,
// attributes or errors: fix the origin first. See redaction.ScrubDocuments for
// the matching rule and its accepted false positives.
func WithDocumentScrubbing() TelemetryOption {
	return telemetryOptionFunc(func(opts *telemetryOptions) {
		opts.scrubDocuments = true
	})
}

// documentScrubbingExporter scrubs spans on their way to the wrapped exporter.
//
// It is an exporter wrapper rather than a SpanProcessor because OnEnd receives
// an immutable span, and the status and events it must scrub are set after
// OnStart. It holds no state beyond the wrapped exporter, so it is safe for
// concurrent use; Shutdown delegates once per call, and the provider that owns
// the exporter calls it once.
type documentScrubbingExporter struct {
	inner sdktrace.SpanExporter
}

func newDocumentScrubbingExporter(inner sdktrace.SpanExporter) sdktrace.SpanExporter {
	return documentScrubbingExporter{inner: inner}
}

func (e documentScrubbingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if e.inner == nil {
		return nil
	}

	scrubbed := make([]sdktrace.ReadOnlySpan, 0, len(spans))

	for _, span := range spans {
		if span == nil {
			continue
		}

		scrubbed = append(scrubbed, scrubSpan(span))
	}

	return e.inner.ExportSpans(ctx, scrubbed)
}

func (e documentScrubbingExporter) Shutdown(ctx context.Context) error {
	if e.inner == nil {
		return nil
	}

	return e.inner.Shutdown(ctx)
}

// scrubbedSpan overrides the text-bearing accessors of a ReadOnlySpan.
// Embedding the original satisfies the interface's unexported method and keeps
// identity, timing, resource and scope untouched.
type scrubbedSpan struct {
	sdktrace.ReadOnlySpan
	name       string
	status     sdktrace.Status
	attributes []attribute.KeyValue
	events     []sdktrace.Event
	links      []sdktrace.Link
}

func (s scrubbedSpan) Name() string                     { return s.name }
func (s scrubbedSpan) Status() sdktrace.Status          { return s.status }
func (s scrubbedSpan) Attributes() []attribute.KeyValue { return s.attributes }
func (s scrubbedSpan) Events() []sdktrace.Event         { return s.events }
func (s scrubbedSpan) Links() []sdktrace.Link           { return s.links }

// scrubSpan returns span itself when it carries no document, and a
// scrubbedSpan over it otherwise.
func scrubSpan(span sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	name := redaction.ScrubDocuments(span.Name())

	status := span.Status()
	description := redaction.ScrubDocuments(status.Description)

	attributes, attributesChanged := scrubAttributes(span.Attributes())
	events, eventsChanged := scrubEvents(span.Events())
	links, linksChanged := scrubLinks(span.Links())

	if name == span.Name() && description == status.Description &&
		!attributesChanged && !eventsChanged && !linksChanged {
		return span
	}

	status.Description = description

	return scrubbedSpan{
		ReadOnlySpan: span,
		name:         name,
		status:       status,
		attributes:   attributes,
		events:       events,
		links:        links,
	}
}

// scrubAttributes copies kvs on the first change and returns it untouched
// otherwise; the span's own slice is never written.
func scrubAttributes(kvs []attribute.KeyValue) ([]attribute.KeyValue, bool) {
	var out []attribute.KeyValue

	for i, kv := range kvs {
		value, changed := scrubValue(kv.Value)
		if !changed {
			continue
		}

		if out == nil {
			out = slices.Clone(kvs)
		}

		out[i] = attribute.KeyValue{Key: kv.Key, Value: value}
	}

	if out == nil {
		return kvs, false
	}

	return out, true
}

func scrubValue(v attribute.Value) (attribute.Value, bool) {
	switch v.Type() {
	case attribute.STRING:
		scrubbed := redaction.ScrubDocuments(v.AsString())
		if scrubbed == v.AsString() {
			return v, false
		}

		return attribute.StringValue(scrubbed), true
	case attribute.STRINGSLICE:
		return scrubStringSlice(v)
	case attribute.BYTESLICE:
		raw := string(v.AsByteSlice())

		scrubbed := redaction.ScrubDocuments(raw)
		if scrubbed == raw {
			return v, false
		}

		return attribute.ByteSliceValue([]byte(scrubbed)), true
	case attribute.SLICE:
		return scrubValueSlice(v)
	case attribute.MAP:
		entries, changed := scrubAttributes(v.AsMap())
		if !changed {
			return v, false
		}

		return attribute.MapValue(entries...), true
	default:
		return v, false
	}
}

func scrubStringSlice(v attribute.Value) (attribute.Value, bool) {
	values := v.AsStringSlice()

	var out []string

	for i, s := range values {
		scrubbed := redaction.ScrubDocuments(s)
		if scrubbed == s {
			continue
		}

		if out == nil {
			out = slices.Clone(values)
		}

		out[i] = scrubbed
	}

	if out == nil {
		return v, false
	}

	return attribute.StringSliceValue(out), true
}

func scrubValueSlice(v attribute.Value) (attribute.Value, bool) {
	values := v.AsSlice()

	var out []attribute.Value

	for i, element := range values {
		scrubbed, changed := scrubValue(element)
		if !changed {
			continue
		}

		if out == nil {
			out = slices.Clone(values)
		}

		out[i] = scrubbed
	}

	if out == nil {
		return v, false
	}

	return attribute.SliceValue(out...), true
}

func scrubEvents(events []sdktrace.Event) ([]sdktrace.Event, bool) {
	var out []sdktrace.Event

	for i, event := range events {
		name := redaction.ScrubDocuments(event.Name)
		attributes, changed := scrubAttributes(event.Attributes)

		if name == event.Name && !changed {
			continue
		}

		if out == nil {
			out = slices.Clone(events)
		}

		out[i].Name = name
		out[i].Attributes = attributes
	}

	if out == nil {
		return events, false
	}

	return out, true
}

func scrubLinks(links []sdktrace.Link) ([]sdktrace.Link, bool) {
	var out []sdktrace.Link

	for i, link := range links {
		attributes, changed := scrubAttributes(link.Attributes)
		if !changed {
			continue
		}

		if out == nil {
			out = slices.Clone(links)
		}

		out[i].Attributes = attributes
	}

	if out == nil {
		return links, false
	}

	return out, true
}
