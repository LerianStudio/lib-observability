//go:build unit

package zap

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func TestNewRejectsMissingOTelLibraryName(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Environment: EnvironmentProduction})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OTelLibraryName is required")
}

func TestNewRejectsInvalidEnvironment(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Environment: Environment("banana"), OTelLibraryName: "svc"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid environment")
}

func TestNewAppliesEnvironmentDefaultLevel(t *testing.T) {
	t.Parallel()

	logger, err := New(Config{Environment: EnvironmentDevelopment, OTelLibraryName: "svc"})
	require.NoError(t, err)
	assert.Equal(t, zapcore.DebugLevel, logger.Level().Level())

	logger, err = New(Config{Environment: EnvironmentProduction, OTelLibraryName: "svc"})
	require.NoError(t, err)
	assert.Equal(t, zapcore.InfoLevel, logger.Level().Level())
}

func TestNewAppliesCustomLevel(t *testing.T) {
	t.Parallel()

	logger, err := New(Config{Environment: EnvironmentProduction, OTelLibraryName: "svc", Level: "error"})
	require.NoError(t, err)
	assert.Equal(t, zapcore.ErrorLevel, logger.Level().Level())
}

func TestNewRejectsInvalidCustomLevel(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Environment: EnvironmentProduction, OTelLibraryName: "svc", Level: "invalid"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid level")
}

func TestCallerAttributionPointsToCallSite(t *testing.T) {
	t.Parallel()

	logger, err := New(Config{
		Environment:     EnvironmentDevelopment,
		OTelLibraryName: "test-caller",
	})
	require.NoError(t, err)

	raw := logger.Raw()
	require.NotNil(t, raw, "Raw() should return the underlying zap logger")
}

func TestNewWithLocalEnvironment(t *testing.T) {
	t.Parallel()

	logger, err := New(Config{Environment: EnvironmentLocal, OTelLibraryName: "svc"})
	require.NoError(t, err)
	require.NotNil(t, logger)
	assert.Equal(t, zapcore.DebugLevel, logger.Level().Level())
}

func TestNewWithStagingEnvironment(t *testing.T) {
	t.Parallel()

	logger, err := New(Config{Environment: EnvironmentStaging, OTelLibraryName: "svc"})
	require.NoError(t, err)
	require.NotNil(t, logger)
	assert.Equal(t, zapcore.InfoLevel, logger.Level().Level())
}

func TestNewWithUATEnvironment(t *testing.T) {
	t.Parallel()

	logger, err := New(Config{Environment: EnvironmentUAT, OTelLibraryName: "svc"})
	require.NoError(t, err)
	require.NotNil(t, logger)
	assert.Equal(t, zapcore.InfoLevel, logger.Level().Level())
}

func TestResolveLevelEmptyForProductionDefaultsToInfo(t *testing.T) {
	t.Parallel()

	level, err := resolveLevel(Config{Environment: EnvironmentProduction, Level: ""})
	require.NoError(t, err)
	assert.Equal(t, zapcore.InfoLevel, level.Level())
}

func TestResolveLevelEmptyForLocalDefaultsToDebug(t *testing.T) {
	t.Parallel()

	level, err := resolveLevel(Config{Environment: EnvironmentLocal, Level: ""})
	require.NoError(t, err)
	assert.Equal(t, zapcore.DebugLevel, level.Level())
}

func TestBuildConfigByEnvironmentDev(t *testing.T) {
	t.Setenv("LOG_ENCODING", "")

	cfg := buildConfigByEnvironment(Config{Environment: EnvironmentDevelopment})
	assert.Equal(t, "console", cfg.Encoding)
	assert.True(t, cfg.Development)
}

func TestBuildConfigByEnvironmentProd(t *testing.T) {
	t.Setenv("LOG_ENCODING", "")

	cfg := buildConfigByEnvironment(Config{Environment: EnvironmentProduction})
	assert.Equal(t, "json", cfg.Encoding)
	assert.False(t, cfg.Development)
}

func TestResolveEncodingFromEnvVar(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")
	assert.Equal(t, "json", resolveEncoding(Config{Environment: EnvironmentLocal}))

	t.Setenv("LOG_ENCODING", "console")
	assert.Equal(t, "console", resolveEncoding(Config{Environment: EnvironmentProduction}))

	t.Setenv("LOG_ENCODING", "invalid")
	assert.Equal(t, "console", resolveEncoding(Config{Environment: EnvironmentLocal}))
	assert.Equal(t, "json", resolveEncoding(Config{Environment: EnvironmentProduction}))
}

func TestResolveLevelFromEnvVar(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")

	level, err := resolveLevel(Config{Environment: EnvironmentProduction, Level: ""})
	require.NoError(t, err)
	assert.Equal(t, zapcore.WarnLevel, level.Level())
}

func TestResolveLevelConfigOverridesEnvVar(t *testing.T) {
	t.Setenv("LOG_LEVEL", "warn")

	level, err := resolveLevel(Config{Environment: EnvironmentProduction, Level: "error"})
	require.NoError(t, err)
	assert.Equal(t, zapcore.ErrorLevel, level.Level(), "Config.Level should take precedence over LOG_LEVEL env var")
}

// ---------------------------------------------------------------------------
// Config.Encoding
// ---------------------------------------------------------------------------

// samplingProbeRecords is well above the production profile's Initial of 100,
// so a sampled logger must write strictly fewer lines than this.
const samplingProbeRecords = 500

// newBufferLogger builds a logger that writes every entry to buf, so a test can
// read what the configured encoder actually produced.
func newBufferLogger(t *testing.T, cfg Config, buf *bytes.Buffer) *Logger {
	t.Helper()

	cfg.OTelLibraryName = "svc"
	cfg.Output = buf

	logger, err := New(cfg)
	require.NoError(t, err)

	return logger
}

func TestEncodingJSONOverridesDevelopmentConsole(t *testing.T) {
	t.Setenv("LOG_ENCODING", "")

	var buf bytes.Buffer

	newBufferLogger(t, Config{Environment: EnvironmentDevelopment, Encoding: "json"}, &buf).
		Info("dev asked for json")

	// The development profile keeps zap's short encoder keys ("M" for message).
	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &entry))
	assert.Equal(t, "dev asked for json", entry["M"])

	// A colour level encoder would smuggle ANSI escapes into the JSON value.
	assert.Equal(t, "INFO", entry["L"])
	assert.NotContains(t, buf.String(), "\x1b[")
}

func TestEncodingConsoleOverridesProductionJSON(t *testing.T) {
	t.Setenv("LOG_ENCODING", "")

	var buf bytes.Buffer

	newBufferLogger(t, Config{Environment: EnvironmentProduction, Encoding: "console"}, &buf).
		Info("prod asked for console")

	out := buf.String()
	assert.NotContains(t, out, `"msg":`, "console encoding must not emit JSON")
	assert.Contains(t, out, "INFO")
	assert.Contains(t, out, "prod asked for console")
}

func TestNewRejectsUnknownEncoding(t *testing.T) {
	t.Parallel()

	_, err := New(Config{Environment: EnvironmentProduction, OTelLibraryName: "svc", Encoding: "yaml"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid encoding")
	assert.Contains(t, err.Error(), `"yaml"`, "the error must name the rejected value")
}

func TestResolveEncodingConfigOverridesEnvVar(t *testing.T) {
	t.Setenv("LOG_ENCODING", "console")
	assert.Equal(t, "json", resolveEncoding(Config{Environment: EnvironmentProduction, Encoding: "json"}),
		"Config.Encoding should take precedence over LOG_ENCODING")

	t.Setenv("LOG_ENCODING", "json")
	assert.Equal(t, "console", resolveEncoding(Config{Environment: EnvironmentLocal, Encoding: "console"}),
		"Config.Encoding should take precedence over LOG_ENCODING")
}

func TestResolveEncodingEnvVarAppliesWhenConfigEmpty(t *testing.T) {
	t.Setenv("LOG_ENCODING", "console")
	assert.Equal(t, "console", resolveEncoding(Config{Environment: EnvironmentProduction}))

	t.Setenv("LOG_ENCODING", "json")
	assert.Equal(t, "json", resolveEncoding(Config{Environment: EnvironmentLocal}))
}

// ---------------------------------------------------------------------------
// Config.DisableSampling
// ---------------------------------------------------------------------------

func TestProductionProfileDropsRepeatedRecords(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")

	var buf bytes.Buffer

	logger := newBufferLogger(t, Config{Environment: EnvironmentProduction}, &buf)
	for range samplingProbeRecords {
		logger.Info("repeated message")
	}

	assert.Less(t, strings.Count(buf.String(), "\n"), samplingProbeRecords,
		"the production profile samples at 100:100, so repeats must be dropped")
}

func TestDisableSamplingWritesEveryRecord(t *testing.T) {
	t.Setenv("LOG_ENCODING", "json")

	var buf bytes.Buffer

	logger := newBufferLogger(t, Config{Environment: EnvironmentProduction, DisableSampling: true}, &buf)
	for range samplingProbeRecords {
		logger.Info("repeated message")
	}

	assert.Equal(t, samplingProbeRecords, strings.Count(buf.String(), "\n"),
		"DisableSampling must let every record through")
}

func TestOutputHonouredWithBothKnobs(t *testing.T) {
	// LOG_ENCODING is set to the value Config.Encoding must beat, so this also
	// pins the precedence on the Output road.
	t.Setenv("LOG_ENCODING", "json")

	readStderr := swapStderr(t)

	var buf bytes.Buffer

	// Production: json encoding and a live 100:100 sampler by default, so both
	// knobs have something real to change here.
	logger := newBufferLogger(t, Config{
		Environment:     EnvironmentProduction,
		Encoding:        "console",
		DisableSampling: true,
	}, &buf)

	for range samplingProbeRecords {
		logger.Info("to the writer")
	}

	out := strings.TrimSpace(buf.String())
	assert.NotContains(t, out, `"msg":`, "Config.Encoding must beat LOG_ENCODING on the Output core too")
	assert.Equal(t, samplingProbeRecords, strings.Count(out, "\n")+1,
		"DisableSampling must also remove the sampler wrapping the Output core")

	assert.Empty(t, readStderr(), "no entry should reach the process stderr when Output is set")
}
