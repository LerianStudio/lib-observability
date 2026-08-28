//go:build unit

package log

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingUniversal is a Log-ONLY logger: it implements Universal and
// nothing else, which is exactly the case Adapt has to shim.
type recordingUniversal struct {
	entries []universalEntry
}

type universalEntry struct {
	level  int
	msg    string
	fields []Field
}

func (r *recordingUniversal) Log(_ context.Context, level int, msg string, fields ...any) {
	if r == nil {
		return
	}

	r.entries = append(r.entries, universalEntry{level: level, msg: msg, fields: Fields(fields...)})
}

func (r *recordingUniversal) last(t *testing.T) universalEntry {
	t.Helper()
	require.NotEmpty(t, r.entries)

	return r.entries[len(r.entries)-1]
}

func fieldKeys(fields []Field) []string {
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, f.Key)
	}

	return keys
}

// TestAdapt_ReturnsNopForNil covers both an untyped nil and a typed nil, since
// a typed nil is non-nil to `== nil` and would otherwise panic on first use.
func TestAdapt_ReturnsNopForNil(t *testing.T) {
	t.Parallel()

	var typedNil *recordingUniversal

	var typedNilLogger *GoLogger

	tests := []struct {
		name string
		in   Universal
	}{
		{name: "untyped nil", in: nil},
		{name: "typed nil Universal implementation", in: typedNil},
		{name: "typed nil full Logger implementation", in: typedNilLogger},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Adapt(tt.in)
			require.NotNil(t, got)
			assert.IsType(t, &NopLogger{}, got)
			assert.NotPanics(t, func() {
				got.Log(context.Background(), LevelError, "safe")
			})
		})
	}
}

// TestAdapt_ReturnsFullLoggerAsIs is the identity guarantee and it matters:
// wrapping a real Logger would silently replace its native With/WithGroup/
// Enabled/Sync semantics - redaction, encoder behavior, level checks - with
// the shim's approximations. Adapt must hand the value straight back.
func TestAdapt_ReturnsFullLoggerAsIs(t *testing.T) {
	t.Parallel()

	goLogger := &GoLogger{Level: LevelDebug}
	assert.Same(t, goLogger, Adapt(goLogger), "*GoLogger must pass through untouched")

	nop := NewNop()
	assert.Same(t, nop, Adapt(nop), "NopLogger must pass through untouched")

	// A child produced by With is still a full Logger and must also pass through.
	child := goLogger.With(String("k", "v"))
	assert.Same(t, child, Adapt(child))
}

// TestAdapt_NativeLevelSemanticsSurvive is the observable consequence of the
// identity guarantee: a NopLogger reports Enabled(false) for everything, while
// the shim would report true for any defined level. If Adapt wrapped it, this
// assertion flips.
func TestAdapt_NativeLevelSemanticsSurvive(t *testing.T) {
	t.Parallel()

	assert.False(t, Adapt(NewNop()).Enabled(LevelError))
	assert.False(t, Adapt(&GoLogger{Level: LevelError}).Enabled(LevelDebug))
	assert.True(t, Adapt(&GoLogger{Level: LevelDebug}).Enabled(LevelDebug))
}

func TestAdapt_WrapsLogOnlyLogger(t *testing.T) {
	t.Parallel()

	sink := &recordingUniversal{}
	adapted := Adapt(sink)

	require.IsType(t, &universalShim{}, adapted)

	adapted.Log(context.Background(), LevelInfo, "hello", String("k", "v"))

	entry := sink.last(t)
	assert.Equal(t, LevelInfo, entry.level)
	assert.Equal(t, "hello", entry.msg)
	assert.Equal(t, []Field{{Key: "k", Value: "v"}}, entry.fields)
}

func TestAdaptedShim_EnabledMirrorsLevelValid(t *testing.T) {
	t.Parallel()

	adapted := Adapt(&recordingUniversal{})

	tests := []struct {
		name  string
		level int
		want  bool
	}{
		{name: "error", level: LevelError, want: true},
		{name: "warn", level: LevelWarn, want: true},
		{name: "info", level: LevelInfo, want: true},
		{name: "debug", level: LevelDebug, want: true},
		{name: "out of range high", level: 4, want: false},
		{name: "out of range negative", level: -1, want: false},
		{name: "unknown sentinel", level: LevelUnknown, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A Log-only logger exposes no level check, so answering false for
			// a DEFINED level would silently drop entries it would have emitted.
			assert.Equal(t, tt.want, adapted.Enabled(tt.level))
			assert.Equal(t, LevelValid(tt.level), adapted.Enabled(tt.level))
		})
	}
}

func TestAdaptedShim_SyncIsNoop(t *testing.T) {
	t.Parallel()

	assert.NoError(t, Adapt(&recordingUniversal{}).Sync(context.Background()))
}

// TestAdaptedShim_WithReplaysBoundFieldsFirst: a Log-only logger has nowhere
// to store bound fields, so the shim replays them ahead of the caller's own on
// every Log.
func TestAdaptedShim_WithReplaysBoundFieldsFirst(t *testing.T) {
	t.Parallel()

	sink := &recordingUniversal{}
	bound := Adapt(sink).With(String("service", "ledger"), "tenant", "t-1")

	bound.Log(context.Background(), LevelWarn, "msg", Int("status", 500))

	entry := sink.last(t)
	assert.Equal(t, []Field{
		{Key: "service", Value: "ledger"},
		{Key: "tenant", Value: "t-1"},
		{Key: "status", Value: 500},
	}, entry.fields)
}

// TestAdaptedShim_WithGroupEmitsGroupFieldFirst: the group path is carried as
// an ordinary field because a Universal logger has no WithGroup of its own.
func TestAdaptedShim_WithGroupEmitsGroupFieldFirst(t *testing.T) {
	t.Parallel()

	sink := &recordingUniversal{}

	logger := Adapt(sink).
		With(String("bound", "b")).
		WithGroup("http").
		WithGroup("client")

	logger.Log(context.Background(), LevelInfo, "req", String("own", "o"))

	entry := sink.last(t)
	require.Len(t, entry.fields, 3)
	assert.Equal(t, Field{Key: "group", Value: "http.client"}, entry.fields[0],
		"the group field must come first so operators can scope the line")
	assert.Equal(t, []string{"group", "bound", "own"}, fieldKeys(entry.fields))
}

func TestAdaptedShim_WithGroupIgnoresBlankNames(t *testing.T) {
	t.Parallel()

	base := Adapt(&recordingUniversal{})

	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "spaces", in: "   "},
		{name: "tab and newline", in: "\t\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Same posture as GoLogger and the zap adapter: no empty segment,
			// no allocation, receiver returned unchanged.
			assert.Same(t, base, base.WithGroup(tt.in))
		})
	}
}

func TestAdaptedShim_WithGroupSanitizesName(t *testing.T) {
	t.Parallel()

	sink := &recordingUniversal{}
	Adapt(sink).WithGroup("evil\nINJECTED").Log(context.Background(), LevelError, "m")

	entry := sink.last(t)
	require.NotEmpty(t, entry.fields)
	assert.Equal(t, `evil\nINJECTED`, entry.fields[0].Value)
}

// TestAdaptedShim_SiblingsDoNotContaminate is the aliasing regression: With
// and WithGroup must copy, never append into a slice the parent still shares,
// or two children of one parent end up seeing each other's fields.
func TestAdaptedShim_SiblingsDoNotContaminate(t *testing.T) {
	t.Parallel()

	sink := &recordingUniversal{}
	parent := Adapt(sink).With(String("common", "c"))

	left := parent.With(String("left", "l"))
	right := parent.With(String("right", "r"))

	ctx := context.Background()

	left.Log(ctx, LevelInfo, "left")
	assert.Equal(t, []string{"common", "left"}, fieldKeys(sink.last(t).fields))

	right.Log(ctx, LevelInfo, "right")
	assert.Equal(t, []string{"common", "right"}, fieldKeys(sink.last(t).fields))

	parent.Log(ctx, LevelInfo, "parent")
	assert.Equal(t, []string{"common"}, fieldKeys(sink.last(t).fields),
		"the parent must not have absorbed either child's fields")
}

func TestAdaptedShim_SiblingGroupsDoNotContaminate(t *testing.T) {
	t.Parallel()

	sink := &recordingUniversal{}
	parent := Adapt(sink).WithGroup("root")

	left := parent.WithGroup("left")
	right := parent.WithGroup("right")

	ctx := context.Background()

	left.Log(ctx, LevelInfo, "l")
	assert.Equal(t, "root.left", sink.last(t).fields[0].Value)

	right.Log(ctx, LevelInfo, "r")
	assert.Equal(t, "root.right", sink.last(t).fields[0].Value)

	parent.Log(ctx, LevelInfo, "p")
	assert.Equal(t, "root", sink.last(t).fields[0].Value)
}

// TestAdaptedShim_WithAfterGroupKeepsGroupPath covers the cloneStrings path:
// With must carry the already-bound group path onto the child.
func TestAdaptedShim_WithAfterGroupKeepsGroupPath(t *testing.T) {
	t.Parallel()

	sink := &recordingUniversal{}

	Adapt(sink).
		WithGroup("db").
		With(String("table", "accounts")).
		Log(context.Background(), LevelDebug, "query")

	entry := sink.last(t)
	assert.Equal(t, []string{"group", "table"}, fieldKeys(entry.fields))
	assert.Equal(t, "db", entry.fields[0].Value)
}

// TestUniversalShim_NilReceiverIsSafe: observability code must never be the
// reason a request panics, so every shim method guards its receiver.
func TestUniversalShim_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()

	var s *universalShim

	assert.NotPanics(t, func() {
		s.Log(context.Background(), LevelError, "msg", String("k", "v"))
	})
	assert.IsType(t, &NopLogger{}, s.With(String("k", "v")))
	assert.IsType(t, &NopLogger{}, s.WithGroup("g"))
	assert.False(t, s.Enabled(LevelError))
	assert.NoError(t, s.Sync(context.Background()))
}

func TestCloneStrings(t *testing.T) {
	t.Parallel()

	assert.Nil(t, cloneStrings(nil))
	assert.Nil(t, cloneStrings([]string{}))

	src := []string{"a", "b"}
	got := cloneStrings(src)
	require.Equal(t, src, got)

	src[0] = "mutated"
	assert.Equal(t, "a", got[0])
}
