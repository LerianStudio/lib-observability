//go:build unit

package log

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The level constants are UNTYPED on purpose: an untyped constant converts
// implicitly to both int (the Logger.Log parameter) and Level (the GoLogger
// field). These declarations fail to compile if anyone re-types them, which
// is the whole regression this guards.
var (
	_ int   = LevelError
	_ int   = LevelWarn
	_ int   = LevelInfo
	_ int   = LevelDebug
	_ int   = LevelUnknown
	_ Level = LevelError
	_ Level = LevelWarn
	_ Level = LevelInfo
	_ Level = LevelDebug
	_ Level = LevelUnknown
)

func TestFields_Normalization(t *testing.T) {
	t.Parallel()

	err := errors.New("boom")

	var nilFieldPtr *Field

	presentField := Field{Key: "present", Value: 1}

	tests := []struct {
		name string
		args []any
		want []Field
	}{
		{
			name: "no args yields nil",
			args: nil,
			want: nil,
		},
		{
			name: "empty variadic yields nil",
			args: []any{},
			want: nil,
		},
		{
			name: "typed Field is taken as-is",
			args: []any{String("k", "v")},
			want: []Field{{Key: "k", Value: "v"}},
		},
		{
			name: "Field pointer is dereferenced",
			args: []any{&presentField},
			want: []Field{{Key: "present", Value: 1}},
		},
		{
			name: "nil Field pointer is skipped, not rendered as BadKey",
			args: []any{nilFieldPtr},
			want: []Field{},
		},
		{
			name: "nil Field pointer consumes exactly one element",
			args: []any{nilFieldPtr, "k", "v"},
			want: []Field{{Key: "k", Value: "v"}},
		},
		{
			name: "Field slice is flattened in place",
			args: []any{[]Field{String("a", "1"), Int("b", 2)}},
			want: []Field{{Key: "a", Value: "1"}, {Key: "b", Value: 2}},
		},
		{
			name: "empty Field slice contributes nothing but consumes one element",
			args: []any{[]Field{}, "after", "value"},
			want: []Field{{Key: "after", Value: "value"}},
		},
		{
			name: "string key followed by a value becomes a pair",
			args: []any{"user_id", "u-1"},
			want: []Field{{Key: "user_id", Value: "u-1"}},
		},
		{
			name: "string key followed by nil value still pairs",
			args: []any{"maybe", nil},
			want: []Field{{Key: "maybe", Value: nil}},
		},
		{
			name: "trailing string key with no value lands under BadKey",
			args: []any{"orphan"},
			want: []Field{{Key: BadKey, Value: "orphan"}},
		},
		{
			name: "trailing string key after a complete pair lands under BadKey",
			args: []any{"a", 1, "orphan"},
			want: []Field{{Key: "a", Value: 1}, {Key: BadKey, Value: "orphan"}},
		},
		{
			name: "non-string in key position lands under BadKey and consumes one",
			args: []any{42, "a", 1},
			want: []Field{{Key: BadKey, Value: 42}, {Key: "a", Value: 1}},
		},
		{
			name: "untyped nil in key position lands under BadKey",
			args: []any{nil},
			want: []Field{{Key: BadKey, Value: nil}},
		},
		{
			name: "error in key position lands under BadKey",
			args: []any{err},
			want: []Field{{Key: BadKey, Value: err}},
		},
		{
			name: "mixed typed, slice and pair forms interleave",
			args: []any{
				Err(err),
				[]Field{Bool("ok", true)},
				"count", 3,
				&presentField,
				7,
				"tail",
			},
			want: []Field{
				{Key: "error", Value: err},
				{Key: "ok", Value: true},
				{Key: "count", Value: 3},
				{Key: "present", Value: 1},
				{Key: BadKey, Value: 7},
				{Key: BadKey, Value: "tail"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Fields(tt.args...)

			if tt.want == nil {
				assert.Nil(t, got)
				return
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

// TestFields_DoesNotAliasCallerSlice proves a caller can keep mutating the
// []Field it passed without corrupting an already-normalized result.
func TestFields_DoesNotAliasCallerSlice(t *testing.T) {
	t.Parallel()

	src := []Field{String("k", "v")}
	got := Fields(src)
	require.Len(t, got, 1)

	src[0] = String("k", "mutated")

	assert.Equal(t, "v", got[0].Value)
}

func TestAnyFields_RoundTrip(t *testing.T) {
	t.Parallel()

	assert.Nil(t, anyFields(nil))
	assert.Nil(t, anyFields([]Field{}))

	in := []Field{String("a", "1"), Int("b", 2)}
	widened := anyFields(in)
	require.Len(t, widened, 2)

	assert.Equal(t, in, Fields(widened...))
}

func TestLevelNameAndValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		level    int
		wantName string
		wantOK   bool
	}{
		{name: "error", level: LevelError, wantName: "error", wantOK: true},
		{name: "warn", level: LevelWarn, wantName: "warn", wantOK: true},
		{name: "info", level: LevelInfo, wantName: "info", wantOK: true},
		{name: "debug", level: LevelDebug, wantName: "debug", wantOK: true},
		{name: "unknown sentinel", level: LevelUnknown, wantName: "unknown", wantOK: false},
		{name: "just above debug", level: 4, wantName: "unknown", wantOK: false},
		{name: "negative", level: -1, wantName: "unknown", wantOK: false},
		{name: "far out of range", level: 1 << 20, wantName: "unknown", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.wantName, LevelName(tt.level))
			assert.Equal(t, tt.wantOK, LevelValid(tt.level))
		})
	}
}

// TestLevelString_MatchesLevelName pins the defined-type String() to the
// int-taking LevelName, so the two spellings can never drift.
func TestLevelString_MatchesLevelName(t *testing.T) {
	t.Parallel()

	for _, lvl := range []Level{LevelError, LevelWarn, LevelInfo, LevelDebug, LevelUnknown} {
		assert.Equal(t, LevelName(int(lvl)), lvl.String())
	}
}

func TestBadKeySentinel(t *testing.T) {
	t.Parallel()

	// Same sentinel log/slog uses, so a mixed fleet reads one convention.
	assert.Equal(t, "!BADKEY", BadKey)
	assert.Equal(t, "!BADLEVEL", BadLevelKey)
}
