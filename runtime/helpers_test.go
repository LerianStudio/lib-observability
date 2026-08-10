//go:build unit

package runtime

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LerianStudio/lib-observability/v3/log"
)

// testLogger is a test logger that captures log calls.
// It is shared across all runtime test files.
type testLogger struct {
	mu          sync.Mutex
	errorCalls  []string
	lastMessage string
	panicLogged atomic.Bool
	logged      chan struct{} // Signals when a panic was logged
}

func newTestLogger() *testLogger {
	return &testLogger{
		logged: make(chan struct{}, 1), // Buffered to avoid blocking
	}
}

func (logger *testLogger) Log(_ context.Context, lvl log.Level, msg string, _ ...log.Field) {
	logger.mu.Lock()
	defer logger.mu.Unlock()

	logger.errorCalls = append(logger.errorCalls, msg)
	logger.lastMessage = msg
	if lvl == log.LevelError && strings.Contains(strings.ToLower(msg), "panic") {
		logger.panicLogged.Store(true)
	}

	// Signal that logging occurred (non-blocking)
	select {
	case logger.logged <- struct{}{}:
	default:
	}
}

func (logger *testLogger) wasPanicLogged() bool {
	return logger.panicLogged.Load()
}

func (logger *testLogger) waitForPanicLog(timeout time.Duration) bool {
	select {
	case <-logger.logged:
		return true
	case <-time.After(timeout):
		return false
	}
}
