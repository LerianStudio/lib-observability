//go:build unit

package log

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedEntry is one captured Log call.
type recordedEntry struct {
	level  Level
	msg    string
	fields []Field
}

// recordingLogger is a Logger that captures every call, so the adapter's
// translation (level mapping, kv parsing, With/WithGroup delegation) can be
// asserted on directly.
type recordingLogger struct {
	entries    []recordedEntry
	bound      []Field
	groups     []string
	enabledFor Level
	syncErr    error
	syncCalls  int
}

func newRecordingLogger() *recordingLogger {
	return &recordingLogger{enabledFor: LevelDebug}
}

func (l *recordingLogger) Log(_ context.Context, level Level, msg string, fields ...Field) {
	l.entries = append(l.entries, recordedEntry{level: level, msg: msg, fields: fields})
}

//nolint:ireturn // implements the package Logger interface.
func (l *recordingLogger) With(fields ...Field) Logger {
	l.bound = append(l.bound, fields...)

	return l
}

//nolint:ireturn // implements the package Logger interface.
func (l *recordingLogger) WithGroup(name string) Logger {
	l.groups = append(l.groups, name)

	return l
}

func (l *recordingLogger) Enabled(level Level) bool {
	return l.enabledFor >= level
}

func (l *recordingLogger) Sync(context.Context) error {
	l.syncCalls++

	return l.syncErr
}

func TestContractLevelRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		in       int
		expected Level
		ok       bool
	}{
		{name: "error", in: LevelErrorInt, expected: LevelError, ok: true},
		{name: "warn", in: LevelWarnInt, expected: LevelWarn, ok: true},
		{name: "info", in: LevelInfoInt, expected: LevelInfo, ok: true},
		{name: "debug", in: LevelDebugInt, expected: LevelDebug, ok: true},
		{name: "just above debug", in: 4, expected: LevelUnknown, ok: false},
		{name: "unknown sentinel", in: LevelUnknownInt, expected: LevelUnknown, ok: false},
		{name: "above uint8", in: 256, expected: LevelUnknown, ok: false},
		{name: "negative", in: -1, expected: LevelUnknown, ok: false},
		{name: "far negative", in: -1000, expected: LevelUnknown, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := levelFromInt(tt.in)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestContractIntConstantsMatchLevelScale(t *testing.T) {
	assert.Equal(t, 0, LevelErrorInt)
	assert.Equal(t, 1, LevelWarnInt)
	assert.Equal(t, 2, LevelInfoInt)
	assert.Equal(t, 3, LevelDebugInt)
	assert.Equal(t, 255, LevelUnknownInt)

	// The universal ints must stay pinned to the nominal constants: a
	// consumer hardcoding 0..3 depends on this equality.
	assert.Equal(t, int(LevelError), LevelErrorInt)
	assert.Equal(t, int(LevelWarn), LevelWarnInt)
	assert.Equal(t, int(LevelInfo), LevelInfoInt)
	assert.Equal(t, int(LevelDebug), LevelDebugInt)
}

func TestContractLogForwardsEveryDefinedLevel(t *testing.T) {
	for _, level := range []int{LevelErrorInt, LevelWarnInt, LevelInfoInt, LevelDebugInt} {
		inner := newRecordingLogger()
		AsContract(inner).Log(context.Background(), level, "msg")

		require.Len(t, inner.entries, 1)
		//nolint:gosec // level is one of the four defined constants.
		assert.Equal(t, Level(level), inner.entries[0].level)
		assert.Equal(t, "msg", inner.entries[0].msg)
		assert.Empty(t, inner.entries[0].fields)
	}
}

func TestContractLogOutOfRangeLevelEmitsAtErrorWithBadLevelField(t *testing.T) {
	for _, level := range []int{-1, 4, LevelUnknownInt, 4096} {
		inner := newRecordingLogger()
		AsContract(inner).Log(context.Background(), level, "msg", "k", "v")

		require.Len(t, inner.entries, 1)
		assert.Equal(t, LevelError, inner.entries[0].level, "level %d must not be dropped", level)
		require.Len(t, inner.entries[0].fields, 2)
		assert.Equal(t, Field{Key: "k", Value: "v"}, inner.entries[0].fields[0])
		assert.Equal(t, Field{Key: ContractBadLevelKey, Value: level}, inner.entries[0].fields[1])
	}
}

func TestContractEnabled(t *testing.T) {
	inner := newRecordingLogger()
	inner.enabledFor = LevelInfo

	u := AsContract(inner)

	assert.True(t, u.Enabled(LevelErrorInt))
	assert.True(t, u.Enabled(LevelWarnInt))
	assert.True(t, u.Enabled(LevelInfoInt))
	assert.False(t, u.Enabled(LevelDebugInt))

	// Out-of-range levels are answered here, without consulting the inner logger.
	assert.False(t, u.Enabled(-1))
	assert.False(t, u.Enabled(4))
	assert.False(t, u.Enabled(LevelUnknownInt))
}

func TestContractSyncDelegates(t *testing.T) {
	inner := newRecordingLogger()
	require.NoError(t, AsContract(inner).Sync(context.Background()))
	assert.Equal(t, 1, inner.syncCalls)

	failing := newRecordingLogger()
	failing.syncErr = errors.New("flush failed")
	assert.ErrorContains(t, AsContract(failing).Sync(context.Background()), "flush failed")
}

func TestFieldsFromKV(t *testing.T) {
	tests := []struct {
		name     string
		kv       []any
		expected []Field
	}{
		{name: "nil", kv: nil, expected: nil},
		{name: "empty", kv: []any{}, expected: nil},
		{
			name:     "well formed pairs",
			kv:       []any{"a", 1, "b", "two"},
			expected: []Field{{Key: "a", Value: 1}, {Key: "b", Value: "two"}},
		},
		{
			name:     "odd trailing key",
			kv:       []any{"a", 1, "dangling"},
			expected: []Field{{Key: "a", Value: 1}, {Key: ContractBadKey, Value: "dangling"}},
		},
		{
			name:     "lone key",
			kv:       []any{"dangling"},
			expected: []Field{{Key: ContractBadKey, Value: "dangling"}},
		},
		{
			name:     "non string key consumes one element",
			kv:       []any{42, "a", 1},
			expected: []Field{{Key: ContractBadKey, Value: 42}, {Key: "a", Value: 1}},
		},
		{
			name:     "nil key",
			kv:       []any{nil, "a", 1},
			expected: []Field{{Key: ContractBadKey, Value: nil}, {Key: "a", Value: 1}},
		},
		{
			name:     "nil value is kept",
			kv:       []any{"a", nil},
			expected: []Field{{Key: "a", Value: nil}},
		},
		{
			name:     "empty string key is kept as a key",
			kv:       []any{"", 1},
			expected: []Field{{Key: "", Value: 1}},
		},
		{
			name:     "only bad keys",
			kv:       []any{1, 2, 3},
			expected: []Field{{Key: ContractBadKey, Value: 1}, {Key: ContractBadKey, Value: 2}, {Key: ContractBadKey, Value: 3}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, fieldsFromKV(tt.kv))
		})
	}
}

func TestContractWithDelegatesToInnerLogger(t *testing.T) {
	inner := newRecordingLogger()

	u := ContractWith(AsContract(inner), "service", "ledger", "attempt", 2)
	u.Log(context.Background(), LevelInfoInt, "msg")

	assert.Equal(t, []Field{{Key: "service", Value: "ledger"}, {Key: "attempt", Value: 2}}, inner.bound)
	require.Len(t, inner.entries, 1)
	assert.Empty(t, inner.entries[0].fields, "bound fields belong to the Logger, not the entry")
}

func TestContractWithGroupDelegatesToInnerLogger(t *testing.T) {
	inner := newRecordingLogger()

	u := ContractWithGroup(AsContract(inner), "http")
	u = ContractWithGroup(u, "server")
	u.Log(context.Background(), LevelInfoInt, "msg")

	assert.Equal(t, []string{"http", "server"}, inner.groups)
}

func TestContractWithNoOpArguments(t *testing.T) {
	inner := newRecordingLogger()
	u := AsContract(inner)

	assert.Same(t, u, ContractWith(u), "empty kv returns the same logger")
	assert.Same(t, u, ContractWithGroup(u, ""), "empty group name returns the same logger")
	assert.Same(t, u, ContractWithGroup(u, "   "), "whitespace group name returns the same logger")
	assert.Empty(t, inner.bound)
	assert.Empty(t, inner.groups)
}

// foreignContract is a Contract that is not one of ours: it exercises
// the documented degradation path of ContractWith/ContractWithGroup.
type foreignContract struct {
	calls   [][]any
	levels  []int
	enabled bool
	syncs   int
}

func (f *foreignContract) Log(_ context.Context, level int, _ string, kv ...any) {
	f.levels = append(f.levels, level)
	f.calls = append(f.calls, kv)
}

func (f *foreignContract) Enabled(int) bool { return f.enabled }

func (f *foreignContract) Sync(context.Context) error {
	f.syncs++

	return nil
}

func TestContractWithForeignImplementationReplaysPairs(t *testing.T) {
	foreign := &foreignContract{enabled: true}

	u := ContractWith(foreign, "service", "ledger")
	u = ContractWith(u, "attempt", 2)
	u = ContractWithGroup(u, "http")
	u = ContractWithGroup(u, "server")

	u.Log(context.Background(), LevelWarnInt, "msg", "request_id", "abc")

	require.Len(t, foreign.calls, 1)
	assert.Equal(t, []any{
		universalGroupKey, "http.server",
		"service", "ledger",
		"attempt", 2,
		"request_id", "abc",
	}, foreign.calls[0])
	assert.Equal(t, []int{LevelWarnInt}, foreign.levels)

	assert.True(t, u.Enabled(LevelDebugInt))
	require.NoError(t, u.Sync(context.Background()))
	assert.Equal(t, 1, foreign.syncs)
}

func TestContractWithForeignImplementationDoesNotMutateParent(t *testing.T) {
	foreign := &foreignContract{}

	base := ContractWith(foreign, "service", "ledger")
	child := ContractWith(base, "attempt", 2)

	base.Log(context.Background(), LevelInfoInt, "base")
	child.Log(context.Background(), LevelInfoInt, "child")

	require.Len(t, foreign.calls, 2)
	assert.Equal(t, []any{"service", "ledger"}, foreign.calls[0])
	assert.Equal(t, []any{"service", "ledger", "attempt", 2}, foreign.calls[1])
}

func TestContractNilSafety(t *testing.T) {
	ctx := context.Background()

	// Untyped nil Logger.
	u := AsContract(nil)
	require.NotNil(t, u)
	assert.NotPanics(t, func() { u.Log(ctx, LevelInfoInt, "msg", "k", "v") })
	assert.False(t, u.Enabled(LevelErrorInt))
	require.NoError(t, u.Sync(ctx))

	// Logger interface holding a typed-nil implementation.
	var typedNil *GoLogger

	var asInterface Logger = typedNil

	typedNilAdapter := AsContract(asInterface)
	require.NotNil(t, typedNilAdapter)
	assert.NotPanics(t, func() { typedNilAdapter.Log(ctx, LevelErrorInt, "msg") })
	assert.False(t, typedNilAdapter.Enabled(LevelErrorInt))
	require.NoError(t, typedNilAdapter.Sync(ctx))

	// Nil Contract passed to the free functions.
	require.NotNil(t, ContractWith(nil, "k", "v"))
	require.NotNil(t, ContractWithGroup(nil, "group"))
	assert.NotPanics(t, func() {
		ContractWith(nil, "k", "v").Log(ctx, LevelInfoInt, "msg")
		ContractWithGroup(nil, "group").Log(ctx, LevelInfoInt, "msg")
	})

	// Typed-nil Contract passed to the free functions.
	var typedNilForeign *foreignContract

	var asContract Contract = typedNilForeign

	assert.NotPanics(t, func() {
		ContractWith(asContract, "k", "v").Log(ctx, LevelInfoInt, "msg")
		ContractWithGroup(asContract, "group").Log(ctx, LevelInfoInt, "msg")
	})
}

func TestContractOverNopLoggerDropsEverything(t *testing.T) {
	u := AsContract(NewNop())

	assert.False(t, u.Enabled(LevelErrorInt))
	assert.NotPanics(t, func() { u.Log(context.Background(), LevelErrorInt, "msg") })
	require.NoError(t, u.Sync(context.Background()))
	require.NotNil(t, ContractWith(u, "k", "v"))
	require.NotNil(t, ContractWithGroup(u, "g"))
}
