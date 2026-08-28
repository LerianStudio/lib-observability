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

func TestUniversalLevelRoundTrip(t *testing.T) {
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

func TestUniversalIntConstantsMatchLevelScale(t *testing.T) {
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

func TestUniversalLogForwardsEveryDefinedLevel(t *testing.T) {
	for _, level := range []int{LevelErrorInt, LevelWarnInt, LevelInfoInt, LevelDebugInt} {
		inner := newRecordingLogger()
		Universal(inner).Log(context.Background(), level, "msg")

		require.Len(t, inner.entries, 1)
		//nolint:gosec // level is one of the four defined constants.
		assert.Equal(t, Level(level), inner.entries[0].level)
		assert.Equal(t, "msg", inner.entries[0].msg)
		assert.Empty(t, inner.entries[0].fields)
	}
}

func TestUniversalLogOutOfRangeLevelEmitsAtErrorWithBadLevelField(t *testing.T) {
	for _, level := range []int{-1, 4, LevelUnknownInt, 4096} {
		inner := newRecordingLogger()
		Universal(inner).Log(context.Background(), level, "msg", "k", "v")

		require.Len(t, inner.entries, 1)
		assert.Equal(t, LevelError, inner.entries[0].level, "level %d must not be dropped", level)
		require.Len(t, inner.entries[0].fields, 2)
		assert.Equal(t, Field{Key: "k", Value: "v"}, inner.entries[0].fields[0])
		assert.Equal(t, Field{Key: UniversalBadLevelKey, Value: level}, inner.entries[0].fields[1])
	}
}

func TestUniversalEnabled(t *testing.T) {
	inner := newRecordingLogger()
	inner.enabledFor = LevelInfo

	u := Universal(inner)

	assert.True(t, u.Enabled(LevelErrorInt))
	assert.True(t, u.Enabled(LevelWarnInt))
	assert.True(t, u.Enabled(LevelInfoInt))
	assert.False(t, u.Enabled(LevelDebugInt))

	// Out-of-range levels are answered here, without consulting the inner logger.
	assert.False(t, u.Enabled(-1))
	assert.False(t, u.Enabled(4))
	assert.False(t, u.Enabled(LevelUnknownInt))
}

func TestUniversalSyncDelegates(t *testing.T) {
	inner := newRecordingLogger()
	require.NoError(t, Universal(inner).Sync(context.Background()))
	assert.Equal(t, 1, inner.syncCalls)

	failing := newRecordingLogger()
	failing.syncErr = errors.New("flush failed")
	assert.ErrorContains(t, Universal(failing).Sync(context.Background()), "flush failed")
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
			expected: []Field{{Key: "a", Value: 1}, {Key: UniversalBadKey, Value: "dangling"}},
		},
		{
			name:     "lone key",
			kv:       []any{"dangling"},
			expected: []Field{{Key: UniversalBadKey, Value: "dangling"}},
		},
		{
			name:     "non string key consumes one element",
			kv:       []any{42, "a", 1},
			expected: []Field{{Key: UniversalBadKey, Value: 42}, {Key: "a", Value: 1}},
		},
		{
			name:     "nil key",
			kv:       []any{nil, "a", 1},
			expected: []Field{{Key: UniversalBadKey, Value: nil}, {Key: "a", Value: 1}},
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
			expected: []Field{{Key: UniversalBadKey, Value: 1}, {Key: UniversalBadKey, Value: 2}, {Key: UniversalBadKey, Value: 3}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, fieldsFromKV(tt.kv))
		})
	}
}

func TestUniversalWithDelegatesToInnerLogger(t *testing.T) {
	inner := newRecordingLogger()

	u := UniversalWith(Universal(inner), "service", "ledger", "attempt", 2)
	u.Log(context.Background(), LevelInfoInt, "msg")

	assert.Equal(t, []Field{{Key: "service", Value: "ledger"}, {Key: "attempt", Value: 2}}, inner.bound)
	require.Len(t, inner.entries, 1)
	assert.Empty(t, inner.entries[0].fields, "bound fields belong to the Logger, not the entry")
}

func TestUniversalWithGroupDelegatesToInnerLogger(t *testing.T) {
	inner := newRecordingLogger()

	u := UniversalWithGroup(Universal(inner), "http")
	u = UniversalWithGroup(u, "server")
	u.Log(context.Background(), LevelInfoInt, "msg")

	assert.Equal(t, []string{"http", "server"}, inner.groups)
}

func TestUniversalWithNoOpArguments(t *testing.T) {
	inner := newRecordingLogger()
	u := Universal(inner)

	assert.Same(t, u, UniversalWith(u), "empty kv returns the same logger")
	assert.Same(t, u, UniversalWithGroup(u, ""), "empty group name returns the same logger")
	assert.Same(t, u, UniversalWithGroup(u, "   "), "whitespace group name returns the same logger")
	assert.Empty(t, inner.bound)
	assert.Empty(t, inner.groups)
}

// foreignUniversal is a UniversalLogger that is not one of ours: it exercises
// the documented degradation path of UniversalWith/UniversalWithGroup.
type foreignUniversal struct {
	calls   [][]any
	levels  []int
	enabled bool
	syncs   int
}

func (f *foreignUniversal) Log(_ context.Context, level int, _ string, kv ...any) {
	f.levels = append(f.levels, level)
	f.calls = append(f.calls, kv)
}

func (f *foreignUniversal) Enabled(int) bool { return f.enabled }

func (f *foreignUniversal) Sync(context.Context) error {
	f.syncs++

	return nil
}

func TestUniversalWithForeignImplementationReplaysPairs(t *testing.T) {
	foreign := &foreignUniversal{enabled: true}

	u := UniversalWith(foreign, "service", "ledger")
	u = UniversalWith(u, "attempt", 2)
	u = UniversalWithGroup(u, "http")
	u = UniversalWithGroup(u, "server")

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

func TestUniversalWithForeignImplementationDoesNotMutateParent(t *testing.T) {
	foreign := &foreignUniversal{}

	base := UniversalWith(foreign, "service", "ledger")
	child := UniversalWith(base, "attempt", 2)

	base.Log(context.Background(), LevelInfoInt, "base")
	child.Log(context.Background(), LevelInfoInt, "child")

	require.Len(t, foreign.calls, 2)
	assert.Equal(t, []any{"service", "ledger"}, foreign.calls[0])
	assert.Equal(t, []any{"service", "ledger", "attempt", 2}, foreign.calls[1])
}

func TestUniversalNilSafety(t *testing.T) {
	ctx := context.Background()

	// Untyped nil Logger.
	u := Universal(nil)
	require.NotNil(t, u)
	assert.NotPanics(t, func() { u.Log(ctx, LevelInfoInt, "msg", "k", "v") })
	assert.False(t, u.Enabled(LevelErrorInt))
	require.NoError(t, u.Sync(ctx))

	// Logger interface holding a typed-nil implementation.
	var typedNil *GoLogger

	var asInterface Logger = typedNil

	typedNilAdapter := Universal(asInterface)
	require.NotNil(t, typedNilAdapter)
	assert.NotPanics(t, func() { typedNilAdapter.Log(ctx, LevelErrorInt, "msg") })
	assert.False(t, typedNilAdapter.Enabled(LevelErrorInt))
	require.NoError(t, typedNilAdapter.Sync(ctx))

	// Nil UniversalLogger passed to the free functions.
	require.NotNil(t, UniversalWith(nil, "k", "v"))
	require.NotNil(t, UniversalWithGroup(nil, "group"))
	assert.NotPanics(t, func() {
		UniversalWith(nil, "k", "v").Log(ctx, LevelInfoInt, "msg")
		UniversalWithGroup(nil, "group").Log(ctx, LevelInfoInt, "msg")
	})

	// Typed-nil UniversalLogger passed to the free functions.
	var typedNilForeign *foreignUniversal

	var asUniversal UniversalLogger = typedNilForeign

	assert.NotPanics(t, func() {
		UniversalWith(asUniversal, "k", "v").Log(ctx, LevelInfoInt, "msg")
		UniversalWithGroup(asUniversal, "group").Log(ctx, LevelInfoInt, "msg")
	})
}

func TestUniversalOverNopLoggerDropsEverything(t *testing.T) {
	u := Universal(NewNop())

	assert.False(t, u.Enabled(LevelErrorInt))
	assert.NotPanics(t, func() { u.Log(context.Background(), LevelErrorInt, "msg") })
	require.NoError(t, u.Sync(context.Background()))
	require.NotNil(t, UniversalWith(u, "k", "v"))
	require.NotNil(t, UniversalWithGroup(u, "g"))
}
