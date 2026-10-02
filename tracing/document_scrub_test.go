//go:build unit

package tracing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/redaction"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	spanTestCPF  = "529.982.247-25"
	spanTestCNPJ = "12.345.678/0001-95"
)

// buildScrubTelemetry wires real providers over exp, the way initExporters does
// once the OTLP exporters exist.
func buildScrubTelemetry(t *testing.T, exp sdktrace.SpanExporter, options ...TelemetryOption) *Telemetry {
	t.Helper()

	resolved := telemetryOptions{}
	for _, option := range options {
		option.apply(&resolved)
	}

	tl, err := buildTelemetry(context.Background(), TelemetryConfig{
		LibraryName: "scrub-test",
		ServiceName: "scrub-test",
		Logger:      log.NewNop(),
		Redactor:    NewDefaultRedactor(),
	}, resolved, exp, &countingMetricExporter{}, &countingLogExporter{})
	require.NoError(t, err)

	return tl
}

// emitDocumentSpan records a document on every exported text surface of a span.
func emitDocumentSpan(tl *Telemetry) {
	_, span := tl.TracerProvider.Tracer("scrub-test").Start(context.Background(), "lookup 52998224725")
	span.SetAttributes(
		attribute.String("note", "payer "+spanTestCPF),
		attribute.StringSlice("docs", []string{"ok", spanTestCNPJ}),
		attribute.Int64("attempt", 3),
		attribute.String("holder", "cnpj 12abc34501de35"),
	)
	span.AddEvent("lookup.miss", trace.WithAttributes(attribute.String("detail", "cnpj "+spanTestCNPJ)))
	HandleSpanError(span, "lookup failed", fmt.Errorf("doc %s not found", spanTestCPF))
	span.End()
}

// spanText flattens every exported text surface of the recorded spans.
func spanText(stubs tracetest.SpanStubs) string {
	var b strings.Builder

	for _, s := range stubs {
		b.WriteString(s.Name + "|" + s.Status.Description + "|")

		for _, kv := range s.Attributes {
			b.WriteString(string(kv.Key) + "=" + kv.Value.Emit() + "|")
		}

		for _, ev := range s.Events {
			b.WriteString(ev.Name + "|")

			for _, kv := range ev.Attributes {
				b.WriteString(string(kv.Key) + "=" + kv.Value.Emit() + "|")
			}
		}
	}

	return b.String()
}

func TestWithDocumentScrubbing_ScrubsEveryExportedTextSurface(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tl := buildScrubTelemetry(t, exp, WithDocumentScrubbing())
	t.Cleanup(func() { _ = tl.ShutdownTelemetryWithContext(context.Background()) })

	emitDocumentSpan(tl)
	require.NoError(t, tl.ForceFlush(context.Background()))

	stubs := exp.GetSpans()
	require.Len(t, stubs, 1)

	got := spanText(stubs)
	assert.NotContains(t, got, spanTestCPF)
	assert.NotContains(t, got, spanTestCNPJ)
	assert.NotContains(t, got, "52998224725")

	span := stubs[0]
	assert.Equal(t, "lookup "+redaction.DocumentPlaceholder, span.Name)
	assert.Equal(t, codes.Error, span.Status.Code, "the status code survives the scrub")
	assert.Contains(t, span.Status.Description, redaction.DocumentPlaceholder)

	attrs := map[attribute.Key]attribute.Value{}
	for _, kv := range span.Attributes {
		attrs[kv.Key] = kv.Value
	}

	assert.Equal(t, "payer "+redaction.DocumentPlaceholder, attrs["note"].AsString())
	assert.Equal(t, []string{"ok", redaction.DocumentPlaceholder}, attrs["docs"].AsStringSlice())
	assert.Equal(t, "cnpj "+redaction.DocumentPlaceholder, attrs["holder"].AsString(), "a lowercase CNPJ after a label is scrubbed")
	assert.Equal(t, int64(3), attrs["attempt"].AsInt64(), "non-string attributes pass through")

	var exceptionMessage string

	for _, ev := range span.Events {
		for _, kv := range ev.Attributes {
			if kv.Key == "exception.message" {
				exceptionMessage = kv.Value.AsString()
			}
		}
	}

	assert.Contains(t, exceptionMessage, redaction.DocumentPlaceholder, "HandleSpanError's exception event is scrubbed")
}

func TestWithDocumentScrubbing_OffKeepsTheText(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tl := buildScrubTelemetry(t, exp)
	t.Cleanup(func() { _ = tl.ShutdownTelemetryWithContext(context.Background()) })

	emitDocumentSpan(tl)
	require.NoError(t, tl.ForceFlush(context.Background()))

	got := spanText(exp.GetSpans())
	assert.Contains(t, got, spanTestCPF, "without the option the exporter sees the text as recorded")
	assert.Contains(t, got, spanTestCNPJ)
	assert.NotContains(t, got, redaction.DocumentPlaceholder)
}

func TestWithDocumentScrubbing_ShutsTheExporterDownOnce(t *testing.T) {
	exp := &countingSpanExporter{}
	tl := buildScrubTelemetry(t, exp, WithDocumentScrubbing())

	require.NoError(t, tl.ShutdownTelemetryWithContext(context.Background()))
	assert.Equal(t, int32(1), exp.shutdowns.Load(), "the provider owns the exporter; the wrapper must not add a second drain")
}

func TestWithDocumentScrubbing_DisabledTelemetryIgnoresTheOption(t *testing.T) {
	tl, err := NewTelemetryWithOptions(TelemetryConfig{
		LibraryName:     "scrub-test",
		Logger:          log.NewNop(),
		EnableTelemetry: false,
	}, WithDocumentScrubbing())
	require.NoError(t, err)
	require.NotNil(t, tl)

	_ = tl.ShutdownTelemetryWithContext(context.Background())
}

// recordingSpanExporter keeps the exact ReadOnlySpan values it was handed.
type recordingSpanExporter struct {
	mu    sync.Mutex
	spans []sdktrace.ReadOnlySpan
	err   error
}

func (e *recordingSpanExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.spans = append(e.spans, spans...)

	return e.err
}

func (*recordingSpanExporter) Shutdown(context.Context) error { return nil }

func recordSpans(t *testing.T, build func(tr trace.Tracer)) []sdktrace.ReadOnlySpan {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	build(tp.Tracer("scrub-test"))

	return sr.Ended()
}

func TestDocumentScrubbingExporter_EdgeCases(t *testing.T) {
	t.Run("nil inner exporter is a no-op", func(t *testing.T) {
		exp := newDocumentScrubbingExporter(nil, nil)

		assert.NoError(t, exp.ExportSpans(context.Background(), nil))
		assert.NoError(t, exp.Shutdown(context.Background()))
	})

	t.Run("nil spans are skipped and clean spans pass through untouched", func(t *testing.T) {
		spans := recordSpans(t, func(tr trace.Tracer) {
			_, clean := tr.Start(context.Background(), "settle order 42")
			clean.SetAttributes(attribute.String("tenant", "a"))
			clean.End()

			_, dirty := tr.Start(context.Background(), "doc "+spanTestCPF)
			dirty.End()
		})
		require.Len(t, spans, 2)

		inner := &recordingSpanExporter{}
		exp := newDocumentScrubbingExporter(inner, nil)

		require.NoError(t, exp.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{nil, spans[0], nil, spans[1]}))
		require.Len(t, inner.spans, 2)
		assert.Same(t, spans[0], inner.spans[0], "a span without documents is exported as the original value")
		assert.Equal(t, "doc "+redaction.DocumentPlaceholder, inner.spans[1].Name())
		assert.Equal(t, spans[1].SpanContext(), inner.spans[1].SpanContext(), "identity is preserved")
	})

	t.Run("the inner exporter error is returned", func(t *testing.T) {
		wantErr := errors.New("collector down")
		exp := newDocumentScrubbingExporter(&recordingSpanExporter{err: wantErr}, nil)

		assert.ErrorIs(t, exp.ExportSpans(context.Background(), nil), wantErr)
	})

	t.Run("link attributes are scrubbed", func(t *testing.T) {
		spans := recordSpans(t, func(tr trace.Tracer) {
			_, parent := tr.Start(context.Background(), "parent")
			parent.End()

			_, span := tr.Start(context.Background(), "child", trace.WithLinks(trace.Link{
				SpanContext: parent.SpanContext(),
				Attributes:  []attribute.KeyValue{attribute.String("why", "doc "+spanTestCNPJ)},
			}))
			span.End()
		})

		inner := &recordingSpanExporter{}
		require.NoError(t, newDocumentScrubbingExporter(inner, nil).ExportSpans(context.Background(), spans))

		links := inner.spans[1].Links()
		require.Len(t, links, 1)
		assert.Equal(t, "doc "+redaction.DocumentPlaceholder, links[0].Attributes[0].Value.AsString())
	})

	t.Run("concurrent exports", func(t *testing.T) {
		spans := recordSpans(t, func(tr trace.Tracer) {
			for range 20 {
				_, s := tr.Start(context.Background(), "doc "+spanTestCPF)
				s.End()
			}
		})

		inner := &recordingSpanExporter{}
		exp := newDocumentScrubbingExporter(inner, nil)

		var wg sync.WaitGroup

		for range 8 {
			wg.Add(1)

			go func() {
				defer wg.Done()

				assert.NoError(t, exp.ExportSpans(context.Background(), spans))
			}()
		}

		wg.Wait()

		require.Len(t, inner.spans, 160)

		for _, s := range inner.spans {
			assert.NotContains(t, s.Name(), spanTestCPF)
		}
	})
}

// spanTestControlNumber is a protocol control number whose shape is also an
// alphanumeric CNPJ's: the reason a consumer exempts its key.
const spanTestControlNumber = "12ABC34501DE35"

func attributeMap(kvs []attribute.KeyValue) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.Value
	}

	return out
}

func TestWithDocumentScrubExemptKeys_KeepsListedAttributes(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tl := buildScrubTelemetry(t, exp, WithDocumentScrubbing(),
		WithDocumentScrubExemptKeys("spb.num_ctrl_if", ""),
		WithDocumentScrubExemptKeys("NUM_CTRL_PART", "raw_ctrl", "group", "items", "exception.message"))
	t.Cleanup(func() { _ = tl.ShutdownTelemetryWithContext(context.Background()) })

	_, span := tl.TracerProvider.Tracer("scrub-test").Start(context.Background(), "ctrl "+spanTestControlNumber)
	span.SetAttributes(
		attribute.String("spb.num_ctrl_if", spanTestControlNumber),
		attribute.StringSlice("num_ctrl_part", []string{"52998224725"}),
		attribute.KeyValue{Key: "raw_ctrl", Value: attribute.ByteSliceValue([]byte(spanTestControlNumber))},
		attribute.String("note", spanTestControlNumber),
		attribute.KeyValue{Key: "envelope", Value: attribute.MapValue(
			attribute.String("spb.num_ctrl_if", spanTestControlNumber),
			attribute.String("note", spanTestControlNumber),
		)},
		attribute.KeyValue{Key: "group", Value: attribute.MapValue(attribute.String("note", spanTestControlNumber))},
		attribute.KeyValue{Key: "items", Value: attribute.SliceValue(attribute.StringValue(spanTestControlNumber))},
	)
	span.AddEvent("ctrl "+spanTestControlNumber, trace.WithAttributes(
		attribute.String("spb.num_ctrl_if", spanTestControlNumber),
		attribute.String("detail", spanTestControlNumber),
	))
	HandleSpanError(span, "ctrl "+spanTestControlNumber, errors.New("ctrl "+spanTestControlNumber))
	span.End()
	require.NoError(t, tl.ForceFlush(context.Background()))

	stubs := exp.GetSpans()
	require.Len(t, stubs, 1)

	got := stubs[0]
	attrs := attributeMap(got.Attributes)

	assert.Equal(t, spanTestControlNumber, attrs["spb.num_ctrl_if"].AsString())
	assert.Equal(t, []string{"52998224725"}, attrs["num_ctrl_part"].AsStringSlice(), "keys match ignoring case")
	assert.Equal(t, spanTestControlNumber, string(attrs["raw_ctrl"].AsByteSlice()))
	assert.Equal(t, redaction.DocumentPlaceholder, attrs["note"].AsString(), "a key not listed is scrubbed")

	envelope := attributeMap(attrs["envelope"].AsMap())
	assert.Equal(t, spanTestControlNumber, envelope["spb.num_ctrl_if"].AsString(), "a map entry is judged by its own key")
	assert.Equal(t, redaction.DocumentPlaceholder, envelope["note"].AsString())

	assert.Equal(t, redaction.DocumentPlaceholder, attributeMap(attrs["group"].AsMap())["note"].AsString(),
		"a map under an exempt key is still scrubbed")
	assert.Equal(t, redaction.DocumentPlaceholder, attrs["items"].AsSlice()[0].AsString(),
		"a slice of values under an exempt key is still scrubbed")

	assert.Equal(t, "ctrl "+redaction.DocumentPlaceholder, got.Name, "the span name is free text")
	assert.NotContains(t, got.Status.Description, spanTestControlNumber, "the status is free text")
	assert.Contains(t, got.Status.Description, redaction.DocumentPlaceholder)

	evAttrs := map[attribute.Key]attribute.Value{}

	for _, ev := range got.Events {
		assert.NotContains(t, ev.Name, spanTestControlNumber, "an event name is free text")

		for key, value := range attributeMap(ev.Attributes) {
			evAttrs[key] = value
		}
	}

	require.Contains(t, evAttrs, attribute.Key("exception.message"))
	assert.Equal(t, spanTestControlNumber, evAttrs["spb.num_ctrl_if"].AsString())
	assert.Equal(t, redaction.DocumentPlaceholder, evAttrs["detail"].AsString())
	assert.NotContains(t, evAttrs["exception.message"].AsString(), spanTestControlNumber,
		"exception attributes are never exempt")
}

func TestWithDocumentScrubExemptKeys_LinkAttributes(t *testing.T) {
	spans := recordSpans(t, func(tr trace.Tracer) {
		_, parent := tr.Start(context.Background(), "parent")
		parent.End()

		_, span := tr.Start(context.Background(), "child", trace.WithLinks(trace.Link{
			SpanContext: parent.SpanContext(),
			Attributes: []attribute.KeyValue{
				attribute.String("NumCtrlIF", spanTestControlNumber),
				attribute.String("why", spanTestControlNumber),
			},
		}))
		span.End()
	})

	inner := &recordingSpanExporter{}
	require.NoError(t, newDocumentScrubbingExporter(inner, []string{"numctrlif"}).ExportSpans(context.Background(), spans))

	links := inner.spans[1].Links()
	require.Len(t, links, 1)

	attrs := attributeMap(links[0].Attributes)
	assert.Equal(t, spanTestControlNumber, attrs["NumCtrlIF"].AsString())
	assert.Equal(t, redaction.DocumentPlaceholder, attrs["why"].AsString())
}

func TestWithDocumentScrubExemptKeys_WithoutScrubbingChangesNothing(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tl := buildScrubTelemetry(t, exp, WithDocumentScrubExemptKeys("note"))
	t.Cleanup(func() { _ = tl.ShutdownTelemetryWithContext(context.Background()) })

	emitDocumentSpan(tl)
	require.NoError(t, tl.ForceFlush(context.Background()))

	got := spanText(exp.GetSpans())
	assert.Contains(t, got, spanTestCPF, "the list alone installs no scrub")
	assert.NotContains(t, got, redaction.DocumentPlaceholder)
}

func TestWithDocumentScrubExemptKeys_EmptyListKeepsTheFullScrub(t *testing.T) {
	for name, option := range map[string]TelemetryOption{
		"no keys":    WithDocumentScrubExemptKeys(),
		"empty keys": WithDocumentScrubExemptKeys("", ""),
	} {
		t.Run(name, func(t *testing.T) {
			base := tracetest.NewInMemoryExporter()
			baseTL := buildScrubTelemetry(t, base, WithDocumentScrubbing())
			t.Cleanup(func() { _ = baseTL.ShutdownTelemetryWithContext(context.Background()) })

			listed := tracetest.NewInMemoryExporter()
			listedTL := buildScrubTelemetry(t, listed, WithDocumentScrubbing(), option)
			t.Cleanup(func() { _ = listedTL.ShutdownTelemetryWithContext(context.Background()) })

			emitDocumentSpan(baseTL)
			emitDocumentSpan(listedTL)
			require.NoError(t, baseTL.ForceFlush(context.Background()))
			require.NoError(t, listedTL.ForceFlush(context.Background()))

			assert.Equal(t, spanText(base.GetSpans()), spanText(listed.GetSpans()))
			assert.NotContains(t, spanText(listed.GetSpans()), spanTestCPF)
		})
	}
}

func TestWithDocumentScrubExemptKeys_ListIsCopied(t *testing.T) {
	keys := []string{"NumCtrlIF"}
	option := WithDocumentScrubExemptKeys(keys...)
	keys[0] = "why"

	spans := recordSpans(t, func(tr trace.Tracer) {
		_, span := tr.Start(context.Background(), "s")
		span.SetAttributes(attribute.String("NumCtrlIF", spanTestControlNumber), attribute.String("why", spanTestControlNumber))
		span.End()
	})

	resolved := telemetryOptions{}
	option.apply(&resolved)

	exempt := resolved.documentScrubExemptKeys
	inner := &recordingSpanExporter{}
	exp := newDocumentScrubbingExporter(inner, exempt)
	exempt[0] = "why"

	require.NoError(t, exp.ExportSpans(context.Background(), spans))

	attrs := attributeMap(inner.spans[0].Attributes())
	assert.Equal(t, spanTestControlNumber, attrs["NumCtrlIF"].AsString())
	assert.Equal(t, redaction.DocumentPlaceholder, attrs["why"].AsString())
}
