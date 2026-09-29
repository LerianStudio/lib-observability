package zap

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	logpkg "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/redaction"
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
	switch f.Type {
	case zapcore.StringType:
		scrubbed := redaction.ScrubDocuments(f.String)
		if scrubbed == f.String {
			return f, false
		}

		f.String = scrubbed

		return f, true
	case zapcore.ByteStringType:
		b, ok := f.Interface.([]byte)
		if !ok {
			return f, false
		}

		return replaceIfScrubbed(f, string(b))
	case zapcore.ErrorType:
		err, ok := f.Interface.(error)
		if !ok {
			return f, false
		}

		return zap.String(f.Key, redaction.ScrubDocuments(logpkg.SafeErrorMessage(err))), true
	case zapcore.StringerType:
		rendered, ok := renderStringer(f.Interface)
		if !ok {
			return f, false
		}

		return replaceIfScrubbed(f, rendered)
	case zapcore.ObjectMarshalerType, zapcore.ArrayMarshalerType,
		zapcore.InlineMarshalerType, zapcore.ReflectType:
		if _, isContext := f.Interface.(context.Context); isContext {
			// The OTLP bridge reads a context field as the emit context; it is
			// never rendered as text.
			return f, false
		}

		rendered, ok := renderStructured(f)
		if !ok {
			return f, false
		}

		return replaceIfScrubbed(f, rendered)
	default:
		return f, false
	}
}

func replaceIfScrubbed(f zapcore.Field, rendered string) (zapcore.Field, bool) {
	scrubbed := redaction.ScrubDocuments(rendered)
	if scrubbed == rendered {
		return f, false
	}

	return zap.String(f.Key, scrubbed), true
}

// renderStringer calls String under recover: a nil receiver or a panicking
// method leaves the field to zap's own rendering, which reports it.
func renderStringer(v any) (rendered string, ok bool) {
	s, isStringer := v.(fmt.Stringer)
	if !isStringer {
		return "", false
	}

	defer func() {
		if recover() != nil {
			rendered, ok = "", false
		}
	}()

	return s.String(), true
}

// renderStructured renders a structured field to the JSON its encoder would
// produce, going through the field's own AddTo so a zapcore.ObjectMarshaler is
// read through MarshalLogObject, exactly as the sinks read it. A value that
// panics or does not marshal is left to zap, which reports the failure itself.
func renderStructured(f zapcore.Field) (rendered string, ok bool) {
	defer func() {
		if recover() != nil {
			rendered, ok = "", false
		}
	}()

	enc := zapcore.NewMapObjectEncoder()
	f.AddTo(enc)

	var value any = enc.Fields
	if f.Type != zapcore.InlineMarshalerType {
		value = enc.Fields[f.Key]
	}

	b, err := json.Marshal(value)
	if err != nil {
		return "", false
	}

	return string(b), true
}
