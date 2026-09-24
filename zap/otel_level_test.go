//go:build unit

package zap

import (
	"bytes"
	"context"
	"sync"
	"testing"

	logpkg "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.uber.org/zap/zapcore"
)

// These tests touch the process-wide OTel LoggerProvider, so none of them is
// parallel. The boot test must run before any test installs an SDK provider:
// the global delegate forwards to the first provider ever set, for good.

// recordingExporter keeps the body of every record the SDK exports.
type recordingExporter struct {
	mu     sync.Mutex
	bodies []string
}

func (e *recordingExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	for _, r := range records {
		e.bodies = append(e.bodies, r.Body().AsString())
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
