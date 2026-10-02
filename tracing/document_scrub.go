package tracing

import (
	"context"
	"slices"
	"strings"

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

// WithDocumentScrubExemptKeys lists attribute keys whose own value
// WithDocumentScrubbing leaves as recorded: protocol identifiers whose shape is
// also a CPF's or a CNPJ's (a NumCtrlIF, a NumCtrlPart). A key matches when it
// equals a listed one whole, ignoring case (strings.EqualFold); there is no
// prefix, word or snake/camel-case matching, so a namespaced key such as
// spb.num_ctrl_if is listed as written. Repeated options add to the list,
// empty entries are dropped, and the keys are copied, so later edits to the
// caller's slice have no effect.
//
// Only a string, string-slice or byte-slice attribute is exempted, in span,
// event and link attributes and in map entries, each judged by its own key.
// A slice of values or a map under a listed key is still scrubbed, as are the
// span name, the status description, event names and every attribute whose
// key starts with "exception.". Ignored without WithDocumentScrubbing.
func WithDocumentScrubExemptKeys(keys ...string) TelemetryOption {
	kept := make([]string, 0, len(keys))

	for _, key := range keys {
		if key != "" {
			kept = append(kept, key)
		}
	}

	return telemetryOptionFunc(func(opts *telemetryOptions) {
		opts.documentScrubExemptKeys = append(opts.documentScrubExemptKeys, kept...)
	})
}

// documentScrubbingExporter scrubs spans on their way to the wrapped exporter.
//
// It is an exporter wrapper rather than a SpanProcessor because OnEnd receives
// an immutable span, and the status and events it must scrub are set after
// OnStart. It holds no state beyond the wrapped exporter, so it is safe for
// concurrent use; Shutdown delegates once per call, and the provider that owns
// the exporter calls it once. Its exempt list is its own copy, only read.
type documentScrubbingExporter struct {
	inner  sdktrace.SpanExporter
	exempt documentScrubExemption
}

func newDocumentScrubbingExporter(inner sdktrace.SpanExporter, exempt []string) sdktrace.SpanExporter {
	return documentScrubbingExporter{inner: inner, exempt: slices.Clone(exempt)}
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

		scrubbed = append(scrubbed, e.exempt.scrubSpan(span))
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

// documentScrubExemption is the list of attribute keys whose plain-text
// values are left as recorded (WithDocumentScrubExemptKeys).
type documentScrubExemption []string

// exempts reports whether kv's value is left as recorded: a string,
// string-slice or byte-slice value under a listed key that is not an
// exception attribute.
func (x documentScrubExemption) exempts(kv attribute.KeyValue) bool {
	if len(x) == 0 {
		return false
	}

	switch kv.Value.Type() {
	case attribute.STRING, attribute.STRINGSLICE, attribute.BYTESLICE:
	default:
		return false
	}

	const exceptionPrefix = "exception."

	key := string(kv.Key)
	if len(key) >= len(exceptionPrefix) && strings.EqualFold(key[:len(exceptionPrefix)], exceptionPrefix) {
		return false
	}

	return slices.ContainsFunc(x, func(listed string) bool { return strings.EqualFold(listed, key) })
}

// scrubSpan returns span itself when it carries no document, and a
// scrubbedSpan over it otherwise.
func (x documentScrubExemption) scrubSpan(span sdktrace.ReadOnlySpan) sdktrace.ReadOnlySpan {
	name := redaction.ScrubDocuments(span.Name())

	status := span.Status()
	description := redaction.ScrubDocuments(status.Description)

	attributes, attributesChanged := x.scrubAttributes(span.Attributes())
	events, eventsChanged := x.scrubEvents(span.Events())
	links, linksChanged := x.scrubLinks(span.Links())

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
func (x documentScrubExemption) scrubAttributes(kvs []attribute.KeyValue) ([]attribute.KeyValue, bool) {
	var out []attribute.KeyValue

	for i, kv := range kvs {
		if x.exempts(kv) {
			continue
		}

		value, changed := x.scrubValue(kv.Value)
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

func (x documentScrubExemption) scrubValue(v attribute.Value) (attribute.Value, bool) {
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
		return x.scrubValueSlice(v)
	case attribute.MAP:
		entries, changed := x.scrubAttributes(v.AsMap())
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

func (x documentScrubExemption) scrubValueSlice(v attribute.Value) (attribute.Value, bool) {
	values := v.AsSlice()

	var out []attribute.Value

	for i, element := range values {
		scrubbed, changed := x.scrubValue(element)
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

func (x documentScrubExemption) scrubEvents(events []sdktrace.Event) ([]sdktrace.Event, bool) {
	var out []sdktrace.Event

	for i, event := range events {
		name := redaction.ScrubDocuments(event.Name)
		attributes, changed := x.scrubAttributes(event.Attributes)

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

func (x documentScrubExemption) scrubLinks(links []sdktrace.Link) ([]sdktrace.Link, bool) {
	var out []sdktrace.Link

	for i, link := range links {
		attributes, changed := x.scrubAttributes(link.Attributes)
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
