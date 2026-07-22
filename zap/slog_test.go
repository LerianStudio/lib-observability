//go:build unit

package zap

import (
	"context"
	"testing"

	logpkg "github.com/LerianStudio/lib-observability/v2/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestSlog_WritesThroughZapCore(t *testing.T) {
	core, observed := observer.New(zapcore.InfoLevel)
	l := &Logger{logger: zap.New(core)}

	Slog(l).InfoContext(context.Background(), "resolved", "consul", "localhost:8500")

	require.Equal(t, 1, observed.Len())
	entry := observed.All()[0]
	assert.Equal(t, "resolved", entry.Message)
	assert.Equal(t, zapcore.InfoLevel, entry.Level)
	assert.Equal(t, "localhost:8500", entry.ContextMap()["consul"])
}

func TestSlog_NonZapLoggerFallsBackWithoutPanic(t *testing.T) {
	got := Slog(logpkg.NewNop())

	require.NotNil(t, got)
	assert.NotPanics(t, func() {
		got.InfoContext(context.Background(), "discarded")
	})
}

func TestSlog_NilConcreteLoggerIsSafe(t *testing.T) {
	var l *Logger

	got := Slog(l)

	require.NotNil(t, got)
	assert.NotPanics(t, func() {
		got.InfoContext(context.Background(), "no panic on nil receiver")
	})
}
