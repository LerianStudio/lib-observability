//go:build unit

package zap

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"

	logpkg "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	oteltrace "go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// These tests touch the process-wide OTel LoggerProvider, so none of them is
// parallel. The boot test must run before any test installs an SDK provider:
// the global delegate forwards to the first provider ever set, for good.

// recordingExporter keeps the body of every record the SDK exports.
type recordingExporter struct {
	mu       sync.Mutex
	bodies   []string
	attrs    []string
	raw      []string
	traceIDs []string
}

func (e *recordingExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, r := range records {
		e.bodies = append(e.bodies, r.Body().AsString())
		e.traceIDs = append(e.traceIDs, r.TraceID().String())

		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			e.attrs = append(e.attrs, string(kv.Key)+"="+kv.Value.Emit())
			e.raw = append(e.raw, string(kv.Key)+"="+exportedText(kv.Value))

			return true
		})
	}

	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error   { return nil }
func (e *recordingExporter) ForceFlush(context.Context) error { return nil }

func (e *recordingExporter) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.bodies...)
}

func (e *recordingExporter) seenTraceIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.traceIDs...)
}

// seenRawAttrs is each exported attribute with byte values as their raw text,
// which is what the collector receives on the wire: Emit and String render
// bytes as base64, where no document pattern matches.
func (e *recordingExporter) seenRawAttrs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.raw...)
}

// exportedText flattens v with every byte value as its raw text.
func exportedText(v attribute.Value) string {
	switch v.Type() {
	case attribute.BYTESLICE:
		return string(v.AsByteSlice())
	case attribute.SLICE:
		parts := make([]string, 0, len(v.AsSlice()))
		for _, item := range v.AsSlice() {
			parts = append(parts, exportedText(item))
		}

		return "[" + strings.Join(parts, " ") + "]"
	case attribute.MAP:
		parts := make([]string, 0, len(v.AsMap()))
		for _, kv := range v.AsMap() {
			parts = append(parts, string(kv.Key)+":"+exportedText(kv.Value))
		}

		return "{" + strings.Join(parts, " ") + "}"
	default:
		return v.String()
	}
}

func (e *recordingExporter) seenAttrs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	return append([]string(nil), e.attrs...)
}

// Services build the logger before installing the OTel SDK, while the global
// delegate still reports every severity disabled. New must still succeed.
func TestNewSucceedsBeforeAnyOTelProviderIsInstalled(t *testing.T) {
	probe := global.GetLoggerProvider().Logger("probe")
	require.False(t, probe.Enabled(context.Background(), otellog.EnabledParameters{Severity: otellog.SeverityFatal}),
		"precondition: the global OTel provider must still be the disabled delegate; a test installing an SDK provider ran first")

	var buf bytes.Buffer

	logger, err := New(Config{Environment: EnvironmentProduction, Level: "debug", OTelLibraryName: "boot", Output: &buf})
	require.NoError(t, err)

	for _, level := range []int{logpkg.LevelDebug, logpkg.LevelInfo, logpkg.LevelWarn, logpkg.LevelError} {
		assert.NotPanics(t, func() { logger.Log(context.Background(), level, "boot line") })
	}

	assert.Equal(t, 4, bytes.Count(buf.Bytes(), []byte("boot line")))
}

// LOG_LEVEL must govern the OTLP bridge as well as the Output core: an entry
// below the level is exported by neither, and a runtime SetLevel moves both.
func TestConfiguredLevelGatesTheOTelBridge(t *testing.T) {
	prev := global.GetLoggerProvider()
	t.Cleanup(func() { global.SetLoggerProvider(prev) })

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	global.SetLoggerProvider(provider)

	var buf bytes.Buffer

	logger, err := New(Config{Environment: EnvironmentProduction, Level: "info", OTelLibraryName: "t", Output: &buf})
	require.NoError(t, err)

	ctx := context.Background()
	logger.Log(ctx, logpkg.LevelDebug, "debug below level")
	logger.Log(ctx, logpkg.LevelInfo, "info at level")
	require.NoError(t, provider.ForceFlush(ctx))

	assert.Equal(t, []string{"info at level"}, exporter.seen(),
		"the OTLP bridge must export only entries at or above the configured level")
	assert.NotContains(t, buf.String(), "debug below level")
	assert.Contains(t, buf.String(), "info at level")

	logger.Level().SetLevel(zapcore.DebugLevel)
	logger.Log(ctx, logpkg.LevelDebug, "debug after SetLevel")
	require.NoError(t, provider.ForceFlush(ctx))

	assert.Equal(t, []string{"info at level", "debug after SetLevel"}, exporter.seen(),
		"a runtime level change must reach the OTLP bridge")
	assert.Contains(t, buf.String(), "debug after SetLevel")
}

// Config.ScrubDocuments must reach the OTLP bridge, not only the local sink:
// the record body and its attributes leave the process too.
func TestScrubDocumentsReachesTheOTelBridge(t *testing.T) {
	prev := global.GetLoggerProvider()
	t.Cleanup(func() { global.SetLoggerProvider(prev) })

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	global.SetLoggerProvider(provider)

	var buf bytes.Buffer

	logger, err := New(Config{
		Environment: EnvironmentProduction, Level: "info", OTelLibraryName: "t", Output: &buf, ScrubDocuments: true,
	})
	require.NoError(t, err)

	ctx := context.Background()
	logger.With(logpkg.String("payer", "529.982.247-25")).
		Log(ctx, logpkg.LevelError, "payer 52998224725 rejected", logpkg.Err(errors.New("cnpj 12.345.678/0001-95")))
	require.NoError(t, provider.ForceFlush(ctx))

	assert.Equal(t, []string{"payer [REDACTED_DOCUMENT] rejected"}, exporter.seen())

	attrs := strings.Join(exporter.seenAttrs(), " ")
	assert.Contains(t, attrs, "payer=[REDACTED_DOCUMENT]")
	assert.Contains(t, attrs, "error=cnpj [REDACTED_DOCUMENT]")
	assert.NotContains(t, attrs, "529.982.247-25")
	assert.NotContains(t, attrs, "12.345.678/0001-95")
	assert.NotContains(t, buf.String(), "52998224725")
}

// hiddenDocHolder carries a document where encoding/json does not look (an
// unexported field and a json:"-" one) but fmt's %+v does. The OTLP bridge
// renders a reflected struct with %+v, so the scrub must judge that rendering
// as well as the JSON one.
type hiddenDocHolder struct {
	Name   string
	Tagged string `json:"-"`
	secret string
}

type scrubCtxKey struct{}

// A reflected value must be scrubbed on the OTLP bridge as the bridge renders
// it, not only as the local JSON sink does, and a context field must stay the
// bridge's emit context even when its rendering carries a document.
func TestScrubDocumentsCoversTheBridgeRendering(t *testing.T) {
	prev := global.GetLoggerProvider()
	t.Cleanup(func() { global.SetLoggerProvider(prev) })

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	global.SetLoggerProvider(provider)

	var buf bytes.Buffer

	logger, err := New(Config{
		Environment: EnvironmentProduction, Level: "info", OTelLibraryName: "t", Output: &buf,
		ScrubDocuments: true, DisableSampling: true,
	})
	require.NoError(t, err)

	ctx := context.Background()
	unexported := hiddenDocHolder{Name: "x", secret: "529.982.247-25"}
	tagged := hiddenDocHolder{Name: "x", Tagged: "52998224725"}

	logger.Log(ctx, logpkg.LevelInfo, "unexported", logpkg.Any("customer", unexported))
	logger.Log(ctx, logpkg.LevelInfo, "tagged", logpkg.Any("tagged", tagged))
	logger.Log(ctx, logpkg.LevelInfo, "pointer", logpkg.Any("ptr", &tagged))
	logger.Log(ctx, logpkg.LevelInfo, "slice", logpkg.Any("list", []*hiddenDocHolder{&unexported}))
	logger.Log(ctx, logpkg.LevelInfo, "map", logpkg.Any("byName", map[string]hiddenDocHolder{"a": tagged}))

	traceID := oteltrace.TraceID{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
	spanCtx := oteltrace.ContextWithSpanContext(ctx, oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID: traceID, SpanID: oteltrace.SpanID{1, 2, 3, 4, 5, 6, 7, 8}, TraceFlags: oteltrace.FlagsSampled,
	}))
	docCtx := context.WithValue(spanCtx, scrubCtxKey{}, "12.345.678/0001-95")
	logger.Info("with context", zap.Any("ctx", docCtx))

	require.NoError(t, provider.ForceFlush(ctx))

	attrs := strings.Join(exporter.seenAttrs(), " ")
	for _, doc := range []string{"529.982.247-25", "52998224725", "12.345.678/0001-95"} {
		assert.NotContains(t, attrs, doc, "the bridge exported a document")
		assert.NotContains(t, buf.String(), doc, "the local sink wrote a document")
	}

	traceIDs := exporter.seenTraceIDs()
	require.NotEmpty(t, traceIDs)
	assert.Equal(t, traceID.String(), traceIDs[len(traceIDs)-1],
		"a context field must remain the bridge's emit context")
}

// A document carried as an OpenTelemetry attribute value, or as raw bytes,
// reaches the bridge unchanged (otelzap forwards an attribute.Value as is and
// a byte field as bytes), so it must be judged by its text, not by the base64
// its String and JSON forms show, at the top level and nested.
func TestScrubDocumentsCoversAttributeValuesAndBytes(t *testing.T) {
	prev := global.GetLoggerProvider()
	t.Cleanup(func() { global.SetLoggerProvider(prev) })

	exporter := &recordingExporter{}
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(exporter)))
	global.SetLoggerProvider(provider)

	var buf bytes.Buffer

	logger, err := New(Config{
		Environment: EnvironmentProduction, Level: "info", OTelLibraryName: "t", Output: &buf,
		ScrubDocuments: true, DisableSampling: true,
	})
	require.NoError(t, err)

	const cpf = "529.982.247-25"

	cases := map[string]zap.Field{
		"attribute bytes": zap.Any("doc", attribute.ByteSliceValue([]byte(cpf))),
		"attribute bytes pointer": zap.Any("doc", func() *attribute.Value {
			v := attribute.ByteSliceValue([]byte(cpf))

			return &v
		}()),
		"attribute string":       zap.Any("doc", attribute.StringValue(cpf)),
		"attribute string slice": zap.Any("doc", attribute.StringSliceValue([]string{"a", cpf})),
		"attribute slice":        zap.Any("doc", attribute.SliceValue(attribute.IntValue(1), attribute.ByteSliceValue([]byte(cpf)))),
		"attribute map":          zap.Any("doc", attribute.MapValue(attribute.ByteSlice("raw", []byte(cpf)), attribute.String("s", "x"))),
		"attribute map key":      zap.Any("doc", attribute.MapValue(attribute.Int(cpf, 1))),
		"attribute mixed map": zap.Any("doc", attribute.MapValue(
			attribute.String("s", "52998224725"), attribute.ByteSlice("raw", []byte(cpf)))),
		"attribute in a map":   zap.Any("doc", map[string]attribute.Value{"k": attribute.ByteSliceValue([]byte(cpf))}),
		"attribute in a slice": zap.Any("doc", []attribute.Value{attribute.ByteSliceValue([]byte(cpf))}),
		"key values":           zap.Any("doc", []attribute.KeyValue{attribute.ByteSlice("raw", []byte(cpf))}),
		"binary field":         zap.Binary("doc", []byte(cpf)),
		"bytes via log.Any":    zap.Any("doc", []byte("payer "+cpf)),
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(cpf))

	for name, field := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(exporter.seenRawAttrs())
			buf.Reset()

			logger.Info(name, field)
			require.NoError(t, provider.ForceFlush(context.Background()))

			raw := exporter.seenRawAttrs()
			require.Greater(t, len(raw), before, "the entry must still reach the bridge")

			exported := strings.Join(raw[before:], " ")
			for _, leak := range []string{cpf, "52998224725", encoded} {
				assert.NotContains(t, exported, leak, "the bridge exported a document")
				assert.NotContains(t, buf.String(), leak, "the local sink wrote a document")
			}

			assert.Contains(t, exported, "[REDACTED_DOCUMENT]")
		})
	}
}
