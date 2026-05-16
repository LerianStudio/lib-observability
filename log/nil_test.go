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
