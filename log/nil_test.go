//go:build unit

package log

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNopLoggerMethods(t *testing.T) {
	t.Parallel()

	logger := NewNop()
	logger.Log(context.Background(), LevelInfo, "ignored", String("k", "v"))
	assert.Same(t, logger, logger.With(String("child", "value")))
	assert.Same(t, logger, logger.WithGroup("group"))
	assert.False(t, logger.Enabled(LevelError))
	assert.False(t, logger.Enabled(LevelWarn))
	assert.False(t, logger.Enabled(LevelInfo))
	assert.False(t, logger.Enabled(LevelDebug))
	assert.NoError(t, logger.Sync(context.Background()))
}

func TestIsNil_DetectsInterfaceNilWithoutRejectingConcreteLogger(t *testing.T) {
	t.Parallel()

	var typedNil *NopLogger

	tests := []struct {
		name   string
		logger Logger
		want   bool
	}{
		{name: "nil interface", logger: nil, want: true},
		{name: "typed nil logger", logger: typedNil, want: true},
		{name: "concrete logger", logger: NewNop(), want: false},
	}

	for _, tt := range tests {
		testCase := tt
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, testCase.want, IsNil(testCase.logger))
		})
	}
}
