//go:build unit

package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	errTestPanicRecover     = errors.New("test error")
	errOriginalPanicRecover = errors.New("original error")
)

// TestLogPanicWithStack_NilLogger tests that nil logger doesn't cause panic.
func TestLogPanicWithStack_NilLogger(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		logPanicWithStack(nil, "test", "panic value", []byte("stack trace"))
	})
}

// TestLogPanicWithStack_ValidLogger tests logging with a valid logger.
func TestLogPanicWithStack_ValidLogger(t *testing.T) {
	t.Parallel()

	logger := newTestLogger()
	stack := []byte("goroutine 1 [running]:\nmain.main()\n\t/path/to/file.go:10")

	logPanicWithStack(logger, "test-handler", "test panic", stack)

	assert.True(t, logger.wasPanicLogged())
	assert.NotEmpty(t, logger.errorCalls)
}

// TestLogPanicWithStack_DifferentPanicTypes tests various panic value types.
func TestLogPanicWithStack_DifferentPanicTypes(t *testing.T) {
	t.Parallel()

	type customStruct struct {
		Field string
		Code  int
	}

	tests := []struct {
		name       string
		panicValue any
	}{
		{
			name:       "string panic value",
			panicValue: "something went wrong",
		},
		{
			name:       "error panic value",
			panicValue: errTestPanicRecover,
		},
		{
			name:       "int panic value",
			panicValue: 42,
		},
		{
			name:       "struct panic value",
			panicValue: customStruct{Field: "test", Code: 500},
		},
		{
			name:       "nil panic value",
			panicValue: nil,
		},
		{
			name:       "bool panic value",
			panicValue: true,
		},
		{
			name:       "float panic value",
			panicValue: 3.14159,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := newTestLogger()
			require.NotPanics(t, func() {
				logPanicWithStack(logger, "test-handler", tt.panicValue, []byte("stack"))
			})
		})
	}
}

// TestRecoverAndLog_NilLogger tests recovery with nil logger.
func TestRecoverAndLog_NilLogger(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		func() {
			defer RecoverAndLog(nil, "test")
			panic("test panic")
		}()
	})
}

// TestRecoverAndCrash_NilLogger tests RecoverAndCrash with nil logger.
func TestRecoverAndCrash_NilLogger(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		require.NotNil(t, r, "Should re-panic even with nil logger")
	}()

	func() {
		defer RecoverAndCrash(nil, "test")
		panic("test crash nil logger")
	}()

	t.Fatal("Should not reach here")
}

// TestRecoverWithPolicy_NilLogger tests recovery with nil logger.
func TestRecoverWithPolicy_NilLogger(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		func() {
			defer RecoverWithPolicy(nil, "test", KeepRunning)
			panic("nil logger test")
		}()
	})
}

// TestRecoverWithPolicyAndContext_NilCtx tests recovery with nil context.
func TestRecoverWithPolicyAndContext_NilCtx(t *testing.T) {
	t.Parallel()

	logger := newTestLogger()

	var nilCtx context.Context

	require.NotPanics(t, func() {
		func() {
			defer RecoverWithPolicyAndContext(nilCtx, logger, "test", "test", KeepRunning)
			panic("test")
		}()
	})
}

// TestSafeGo_NilFunction tests SafeGo with nil function.
func TestSafeGo_NilFunction(t *testing.T) {
	t.Parallel()

	logger := newTestLogger()

	require.NotPanics(t, func() {
		SafeGo(logger, "test-nil-fn", KeepRunning, nil)
	})
}

// TestSafeGoWithContext_NilFunction tests SafeGoWithContext with nil function.
func TestSafeGoWithContext_NilFunction(t *testing.T) {
	t.Parallel()

	logger := newTestLogger()

	require.NotPanics(t, func() {
		SafeGoWithContext(context.Background(), logger, "test-nil-fn", KeepRunning, nil)
	})
}

// TestToPanicError_ErrorValue tests toPanicError with error panic value.
func TestToPanicError_ErrorValue(t *testing.T) {
	t.Parallel()

	// Test via production mode - production mode returns error type, not message
	errNonProd := toPanicError(errOriginalPanicRecover, false)
	require.NotNil(t, errNonProd)
	assert.Contains(t, errNonProd.Error(), errOriginalPanicRecover.Error())

	errProd := toPanicError(errOriginalPanicRecover, true)
	require.NotNil(t, errProd)
	assert.NotEqual(t, errNonProd.Error(), errProd.Error(), "production mode should sanitize panic details")
}
