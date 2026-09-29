package zap

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	logpkg "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/redaction"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// documentScrubCore replaces CPF/CNPJ-shaped spans in every entry and field
// before they reach the wrapped core. It wraps the whole tee (local sink plus
// OTLP bridge), so one hook covers both sinks and every entry point: Log, the
// zap-typed helpers, With and Raw().
//
// Check only asks whether any wrapped core is enabled; the wrapped core's own
// Check (level gate, sampler, the bridge's per-logger-name decision) runs in
// Write, over the scrubbed entry, on a CheckedEntry of its own. Delegating
// Check directly would hand the wrapped cores to the caller's CheckedEntry,
// and their Write would then receive the raw fields.
//
// The type holds no mutable state; values are copied, never shared.
type documentScrubCore struct {
	zapcore.Core
	// errorOutput receives write failures of the wrapped cores, as the
	// logger's own ErrorOutput would: the inner CheckedEntry reports them
	// itself, since CheckedEntry.Write returns no error to hand back.
	errorOutput zapcore.WriteSyncer
}

func (c documentScrubCore) With(fields []zapcore.Field) zapcore.Core {
	return documentScrubCore{Core: c.Core.With(scrubDocumentFields(fields)), errorOutput: c.errorOutput}
}

func (c documentScrubCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}

	return ce
}

func (c documentScrubCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	ent.Message = redaction.ScrubDocuments(ent.Message)

	inner := c.Core.Check(ent, nil)
	if inner == nil {
		return nil
	}

	inner.ErrorOutput = c.errorOutput
	inner.Write(scrubDocumentFields(fields)...)

	return nil
}

// scrubDocumentFields returns fields with every document-bearing value
// scrubbed. The caller's slice is never modified: it is returned as is when
// nothing matched, and copied on the first change otherwise.
func scrubDocumentFields(fields []zapcore.Field) []zapcore.Field {
	var out []zapcore.Field

	for i := range fields {
		scrubbed, changed := scrubDocumentField(fields[i])
		if !changed {
			continue
		}

		if out == nil {
			out = slices.Clone(fields)
		}

		out[i] = scrubbed
	}

	if out == nil {
		return fields
	}

	return out
}

// scrubDocumentField reports the scrubbed form of f and whether it differs.
//
// An error is always rendered to its (scrubbed) message: errorVerbose and
// errorCauses can repeat the text in forms this function does not parse, so
// they are dropped rather than trusted. Every other rendered kind keeps its
// original field unless a document was found, so an entry without documents
// keeps its shape.
func scrubDocumentField(f zapcore.Field) (zapcore.Field, bool) {
	if ctx, isContext := f.Interface.(context.Context); isContext {
		return scrubContextField(f, ctx)
	}

	if isAttributeValue(f.Interface) {
		// zap takes an attribute.Value for a Stringer, whose String form shows
		// bytes as base64; it is judged, and replaced, by the text it carries.
		return scrubRenderedField(zap.Reflect(f.Key, f.Interface), f)
	}

	switch f.Type {
	case zapcore.StringType:
		scrubbed := redaction.ScrubDocuments(f.String)
		if scrubbed == f.String {
			return f, false
		}

		f.String = scrubbed

		return f, true
	case zapcore.ByteStringType, zapcore.BinaryType:
		// The bridge exports binary as raw bytes; the local sink as base64.
		b, ok := f.Interface.([]byte)
		if !ok {
			return f, false
		}

		scrubbed := redaction.ScrubDocuments(string(b))
		if scrubbed == string(b) {
			return f, false
		}

		return zap.String(f.Key, scrubbed), true
	case zapcore.ErrorType:
		err, ok := f.Interface.(error)
		if !ok {
			return f, false
		}

		return zap.String(f.Key, redaction.ScrubDocuments(logpkg.SafeErrorMessage(err))), true
	case zapcore.StringerType, zapcore.ObjectMarshalerType, zapcore.ArrayMarshalerType,
		zapcore.InlineMarshalerType, zapcore.ReflectType:
		return scrubRenderedField(f, f)
	default:
		return f, false
	}
}

// scrubRenderedField judges f by everything a sink renders from it: the
// entries its own AddTo produces (so a zapcore.ObjectMarshaler is read through
// MarshalLogObject, and the "<key>Error" text zap adds for a failing marshaler
// or a panicking Stringer is included), each read both as the local sink's
// JSON and as the OTLP bridge's rendering, with every attribute value in it
// read as the text it carries. When no document is found, original is kept as
// is, except a Stringer: each sink would ask it again, and a second answer is
// one nobody judged, so the text judged here is written instead (the sinks
// write a Stringer as a string either way). Otherwise the field is replaced by
// an inline object holding each entry as a scrubbed string, so every key the
// sinks would have written is still written.
func scrubRenderedField(f, original zapcore.Field) (zapcore.Field, bool) {
	entries, ok := renderEntries(f)
	if !ok {
		return original, false
	}

	for key, value := range entries {
		if plain, held := plainAttributes(value, 0); held {
			entries[key] = plain
		}
	}

	found := false

	for _, value := range entries {
		if valueHoldsDocument(value) {
			found = true

			break
		}
	}

	if !found && f.Type != zapcore.StringerType {
		return original, false
	}

	scrubbed := make(scrubbedEntries, 0, len(entries))

	for _, key := range slices.Sorted(maps.Keys(entries)) {
		scrubbed = append(scrubbed, scrubbedEntry{key: key, value: scrubbedRendering(entries[key])})
	}

	return zap.Inline(scrubbed), true
}

// scrubbedRendering is what a sink may write for value once a document was
// found in its field: its local rendering scrubbed, or the placeholder alone
// when the value nests past the walk's bound, since what lies deeper was never
// inspected.
func scrubbedRendering(value any) string {
	if _, complete := bridgeRendering(value); !complete {
		return redaction.DocumentPlaceholder
	}

	return redaction.ScrubDocuments(localRendering(value))
}

// scrubContextField keeps a context field a context: the OTLP bridge takes any
// field holding a context.Context as the record's emit context (its trace and
// span) and never renders it, while the local sink writes its String form.
// When that form carries a document, the field is rewrapped so the bridge still
// finds the same context and the local sink writes the scrubbed form.
func scrubContextField(f zapcore.Field, ctx context.Context) (zapcore.Field, bool) {
	entries, ok := renderEntries(f)
	if !ok {
		return f, false
	}

	var text strings.Builder

	for _, key := range slices.Sorted(maps.Keys(entries)) {
		text.WriteString(localRendering(entries[key]))
	}

	scrubbed := redaction.ScrubDocuments(text.String())
	if scrubbed == text.String() {
		return f, false
	}

	return zap.Stringer(f.Key, scrubbedContext{Context: ctx, text: scrubbed}), true
}

// scrubbedContext is a context whose String form is the scrubbed rendering of
// the context it wraps; every other method is the wrapped context's.
type scrubbedContext struct {
	context.Context
	text string
}

func (c scrubbedContext) String() string { return c.text }

type scrubbedEntry struct{ key, value string }

// scrubbedEntries is the replacement of a field that held a document: each
// entry the field rendered, as its scrubbed string.
type scrubbedEntries []scrubbedEntry

func (e scrubbedEntries) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	for _, entry := range e {
		enc.AddString(entry.key, entry.value)
	}

	return nil
}

// renderEntries runs the field's own AddTo against a map encoder, which keeps
// reflected values raw for valueHoldsDocument to read both ways. A value that
// panics is left to zap, which reports (or propagates) the failure itself.
func renderEntries(f zapcore.Field) (entries map[string]any, ok bool) {
	defer func() {
		if recover() != nil {
			entries, ok = nil, false
		}
	}()

	enc := zapcore.NewMapObjectEncoder()
	f.AddTo(enc)

	return enc.Fields, true
}

// valueHoldsDocument reports whether either sink's rendering of value carries
// a document. The two differ for reflected values: encoding/json skips
// unexported and json:"-" fields, the bridge's %+v prints them. A value
// nesting past the walk's bound counts as holding one: it fails closed.
func valueHoldsDocument(value any) bool {
	if s, isString := value.(string); isString {
		return redaction.ScrubDocuments(s) != s
	}

	bridge, complete := bridgeRendering(value)
	if !complete {
		return true
	}

	for _, rendered := range []string{localRendering(value), bridge} {
		if redaction.ScrubDocuments(rendered) != rendered {
			return true
		}
	}

	return false
}

// localRendering is the text the local sink writes for value: a string as is,
// anything else as its JSON, or as the bridge renders it when it has no JSON.
func localRendering(value any) (rendered string) {
	if s, isString := value.(string); isString {
		return s
	}

	defer func() {
		if recover() != nil {
			rendered, _ = bridgeRendering(value)
		}
	}()

	b, err := json.Marshal(value)
	if err != nil {
		rendered, _ = bridgeRendering(value)

		return rendered
	}

	return string(b)
}

// maxBridgeDepth bounds the walk over nested slices, maps and pointers. The
// bridge itself has no bound, so a value nested deeper is exported without
// having been inspected; the field holding it fails closed instead.
const maxBridgeDepth = 32

// bridgeRendering is the text the OTLP bridge (otelzap's convertValue) emits
// for value, one leaf per line so digits of adjacent leaves never join: a
// struct as %+v, slices, arrays, maps and pointers walked element by element.
// complete is false when the walk stopped at maxBridgeDepth.
func bridgeRendering(value any) (rendered string, complete bool) {
	var b strings.Builder

	defer func() {
		if recover() != nil {
			rendered = b.String()
		}
	}()

	complete = writeBridgeValue(&b, value, 0)

	return b.String(), complete
}

// writeBridgeValue writes value's leaves and reports whether it reached all of
// them within maxBridgeDepth.
func writeBridgeValue(b *strings.Builder, value any, depth int) bool {
	if depth > maxBridgeDepth {
		return false
	}

	switch v := value.(type) {
	case nil:
		return true
	case string:
		b.WriteString(v)
	case []byte:
		b.Write(v)
	case error:
		b.WriteString(v.Error())
	case time.Duration:
		b.WriteString(strconv.FormatInt(v.Nanoseconds(), 10))
	case time.Time:
		b.WriteString(strconv.FormatInt(v.UnixNano(), 10))
	default:
		return writeBridgeReflected(b, reflect.ValueOf(value), depth)
	}

	b.WriteByte('\n')

	return true
}

func writeBridgeReflected(b *strings.Builder, v reflect.Value, depth int) bool {
	complete := true

	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			complete = writeBridgeValue(b, v.Index(i).Interface(), depth+1) && complete
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			complete = writeBridgeValue(b, fmt.Sprintf("%+v", iter.Key().Interface()), depth+1) && complete
			complete = writeBridgeValue(b, iter.Value().Interface(), depth+1) && complete
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			complete = writeBridgeValue(b, v.Elem().Interface(), depth+1)
		}
	default:
		fmt.Fprintf(b, "%+v\n", v.Interface())
	}

	return complete
}

// isAttributeValue reports whether value is an OpenTelemetry attribute value,
// directly or through a non-nil pointer.
func isAttributeValue(value any) bool {
	switch v := value.(type) {
	case attribute.Value:
		return true
	case *attribute.Value:
		return v != nil
	default:
		return false
	}
}

// plainAttributes returns value with every OpenTelemetry attribute value in it
// (at the top, or inside slices, arrays, maps and pointers, as the bridge
// walks them) replaced by the text and structure it carries, and whether it
// held any. The bridge forwards an attribute.Value as is, so a byte value is
// exported as its raw bytes, while its String and JSON forms show base64. A
// value holding none is returned as is, so its renderings keep their shape.
// A struct is a leaf: the bridge renders it with %+v.
func plainAttributes(value any, depth int) (any, bool) {
	if depth > maxBridgeDepth {
		return value, false
	}

	switch v := value.(type) {
	case nil, []byte:
		return value, false
	case attribute.Value:
		return plainAttributeValue(v, depth), true
	case attribute.KeyValue:
		return map[string]any{string(v.Key): plainAttributeValue(v.Value, depth)}, true
	}

	rv := reflect.ValueOf(value)

	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		items := make([]any, rv.Len())
		held := false

		for i := range rv.Len() {
			var itemHeld bool

			items[i], itemHeld = plainAttributes(rv.Index(i).Interface(), depth+1)
			held = held || itemHeld
		}

		if held {
			return items, true
		}
	case reflect.Map:
		return plainMapAttributes(rv, depth)
	case reflect.Pointer, reflect.Interface:
		if !rv.IsNil() {
			if elem, held := plainAttributes(rv.Elem().Interface(), depth+1); held {
				return elem, true
			}
		}
	default:
	}

	return value, false
}

// plainAttributeValue is the text and structure v carries: bytes as their
// text, a map as one single-key object per entry (a map value may repeat a
// key, and every entry must be judged), a slice element by element.
func plainAttributeValue(v attribute.Value, depth int) any {
	if depth > maxBridgeDepth {
		return nil
	}

	switch v.Type() {
	case attribute.BYTESLICE:
		return string(v.AsByteSlice())
	case attribute.SLICE:
		items := make([]any, 0, len(v.AsSlice()))
		for _, item := range v.AsSlice() {
			items = append(items, plainAttributeValue(item, depth+1))
		}

		return items
	case attribute.MAP:
		entries := make([]any, 0, len(v.AsMap()))
		for _, kv := range v.AsMap() {
			entries = append(entries, map[string]any{string(kv.Key): plainAttributeValue(kv.Value, depth+1)})
		}

		return entries
	default:
		return v.AsInterface()
	}
}

// plainMapAttributes is plainAttributes for a map: one single-key object per
// entry, ordered by key, so keys that print alike (1 and "1") never collapse
// before every value is judged.
func plainMapAttributes(rv reflect.Value, depth int) (any, bool) {
	type entry struct {
		key  string
		item any
	}

	entries := make([]entry, 0, rv.Len())
	held := false

	iter := rv.MapRange()
	for iter.Next() {
		item, itemHeld := plainAttributes(iter.Value().Interface(), depth+1)
		entries = append(entries, entry{key: fmt.Sprintf("%+v", iter.Key().Interface()), item: item})
		held = held || itemHeld
	}

	if !held {
		return rv.Interface(), false
	}

	slices.SortStableFunc(entries, func(a, b entry) int { return strings.Compare(a.key, b.key) })

	items := make([]any, len(entries))
	for i, e := range entries {
		items[i] = map[string]any{e.key: e.item}
	}

	return items, true
}
