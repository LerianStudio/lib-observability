//go:build unit

package tracing

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	constant "github.com/LerianStudio/lib-observability/v2/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mustRedactor builds a Redactor or fails the test.
func mustRedactor(t *testing.T, rules []RedactionRule, mask string) *Redactor {
	t.Helper()

	r, err := NewRedactor(rules, mask)
	require.NoError(t, err)

	return r
}

// hashVia returns the HMAC-SHA256 hash of v using the given redactor's key.
func hashVia(r *Redactor, v string) string {
	return r.hashString(v)
}

// ===========================================================================
// 1. Redactor construction
// ===========================================================================

func TestNewRedactor_EmptyRules(t *testing.T) {
	t.Parallel()

	r, err := NewRedactor(nil, "")
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Empty(t, r.rules)
	assert.Equal(t, constant.ObfuscatedValue, r.maskValue, "empty mask should fall back to constant")
}

func TestNewRedactor_CustomMaskValue(t *testing.T) {
	t.Parallel()

	r, err := NewRedactor(nil, "REDACTED")
	require.NoError(t, err)
	assert.Equal(t, "REDACTED", r.maskValue)
}

func TestNewRedactor_DefaultActionIsMask(t *testing.T) {
	t.Parallel()

	r, err := NewRedactor([]RedactionRule{
		{FieldPattern: `^foo$`},
	}, "")
	require.NoError(t, err)
	require.Len(t, r.rules, 1)
	assert.Equal(t, RedactionMask, r.rules[0].Action, "blank Action should default to mask")
}

func TestNewRedactor_InvalidFieldPattern(t *testing.T) {
	t.Parallel()

	_, err := NewRedactor([]RedactionRule{
		{FieldPattern: `[invalid`},
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid redaction field pattern at index 0")
}

func TestNewRedactor_InvalidPathPattern(t *testing.T) {
	t.Parallel()

	_, err := NewRedactor([]RedactionRule{
		{PathPattern: `[broken`},
	}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid redaction path pattern at index 0")
}

func TestNewRedactor_MultipleRulesCompileCorrectly(t *testing.T) {
	t.Parallel()

	r, err := NewRedactor([]RedactionRule{
		{FieldPattern: `^password$`, Action: RedactionMask},
		{FieldPattern: `^email$`, Action: RedactionHash},
		{PathPattern: `^session\.token$`, FieldPattern: `^token$`, Action: RedactionDrop},
	}, "***")
	require.NoError(t, err)
	require.Len(t, r.rules, 3)
	assert.NotNil(t, r.rules[0].fieldRegex)
	assert.Nil(t, r.rules[0].pathRegex)
	assert.NotNil(t, r.rules[2].fieldRegex)
	assert.NotNil(t, r.rules[2].pathRegex)
}

// ===========================================================================
// 2. NewDefaultRedactor
// ===========================================================================

func TestNewDefaultRedactor_IsNotNil(t *testing.T) {
	t.Parallel()

	r := NewDefaultRedactor()
	require.NotNil(t, r)
	assert.NotEmpty(t, r.rules, "default redactor should have rules from DefaultSensitiveFields")
	assert.Equal(t, constant.ObfuscatedValue, r.maskValue)
}

func TestNewDefaultRedactor_MatchesSensitiveFields(t *testing.T) {
	t.Parallel()

	r := NewDefaultRedactor()

	for _, field := range []string{"password", "token", "secret", "authorization", "apikey", "cvv", "ssn"} {
		action, matched := r.actionFor("", field)
		assert.True(t, matched, "field %q should match default rules", field)
		assert.Equal(t, RedactionMask, action)
	}
}

func TestNewDefaultRedactor_CaseInsensitive(t *testing.T) {
	t.Parallel()

	r := NewDefaultRedactor()

	for _, field := range []string{"Password", "PASSWORD", "pAsSwOrD"} {
		_, matched := r.actionFor("", field)
		assert.True(t, matched, "field %q should match case-insensitively", field)
	}
}

func TestNewDefaultRedactor_NonSensitiveFieldUnchanged(t *testing.T) {
	t.Parallel()

	r := NewDefaultRedactor()

	_, matched := r.actionFor("", "username")
	assert.False(t, matched)
}

// ===========================================================================
// 3. actionFor (field and path matching)
// ===========================================================================

func TestActionFor_NilRedactor(t *testing.T) {
	t.Parallel()

	var r *Redactor

	action, matched := r.actionFor("any.path", "any")
	assert.False(t, matched)
	assert.Equal(t, RedactionAction(""), action)
}

func TestActionFor_ExactFieldMatch(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `^email$`, Action: RedactionHash},
	}, "")

	action, ok := r.actionFor("user.email", "email")
	assert.True(t, ok)
	assert.Equal(t, RedactionHash, action)
}

func TestActionFor_RegexFieldPattern(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i).*password.*`, Action: RedactionMask},
	}, "")

	tests := []struct {
		field   string
		matched bool
	}{
		{"password", true},
		{"user_password", true},
		{"password_hash", true},
		{"newPassword", true},
		{"username", false},
	}

	for _, tt := range tests {
		action, ok := r.actionFor("", tt.field)
		assert.Equal(t, tt.matched, ok, "field=%q", tt.field)

		if tt.matched {
			assert.Equal(t, RedactionMask, action)
		}
	}
}

func TestActionFor_PathPatternOnly(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{PathPattern: `^config\.db\.password$`, Action: RedactionDrop},
	}, "")

	_, ok := r.actionFor("config.db.password", "password")
	assert.True(t, ok, "path+field should match")

	_, ok = r.actionFor("user.password", "password")
	assert.False(t, ok, "path should NOT match different prefix")
}

func TestActionFor_CombinedFieldAndPathPattern(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `^token$`, PathPattern: `^session\.`, Action: RedactionDrop},
	}, "")

	_, ok := r.actionFor("session.token", "token")
	assert.True(t, ok)

	_, ok = r.actionFor("auth.token", "token")
	assert.False(t, ok)
}

func TestActionFor_NoMatch(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `^secret$`, Action: RedactionMask},
	}, "")

	_, ok := r.actionFor("", "name")
	assert.False(t, ok)
}

func TestActionFor_FirstMatchWins(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionHash},
		{FieldPattern: `(?i)^password$`, Action: RedactionDrop},
	}, "")

	action, ok := r.actionFor("", "password")
	assert.True(t, ok)
	assert.Equal(t, RedactionHash, action, "first matching rule should win")
}

// ===========================================================================
// 4. redactValue (mask / hash / drop)
// ===========================================================================

func TestRedactValue_Mask(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	val, drop, applied := r.redactValue("", "password", "secret123")
	assert.False(t, drop)
	assert.True(t, applied)
	assert.Equal(t, "***", val)
}

func TestRedactValue_Hash(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^email$`, Action: RedactionHash},
	}, "")

	val, drop, applied := r.redactValue("", "email", "alice@example.com")
	assert.False(t, drop)
	assert.True(t, applied)
	assert.Equal(t, hashVia(r, "alice@example.com"), val)
}

func TestRedactValue_Hash_Deterministic(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^document$`, Action: RedactionHash},
	}, "")

	val1, _, _ := r.redactValue("", "document", "12345")
	val2, _, _ := r.redactValue("", "document", "12345")
	assert.Equal(t, val1, val2, "hashing the same value must be deterministic")
}

func TestRedactValue_Hash_DifferentInputs(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^document$`, Action: RedactionHash},
	}, "")

	val1, _, _ := r.redactValue("", "document", "abc")
	val2, _, _ := r.redactValue("", "document", "def")
	assert.NotEqual(t, val1, val2, "different inputs must produce different hashes")
}

func TestRedactValue_Drop(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^token$`, Action: RedactionDrop},
	}, "")

	val, drop, applied := r.redactValue("", "token", "tok_abc")
	assert.True(t, drop)
	assert.True(t, applied)
	assert.Nil(t, val)
}

func TestRedactValue_NoMatch(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `^secret$`, Action: RedactionMask},
	}, "")

	val, drop, applied := r.redactValue("", "name", "Alice")
	assert.False(t, drop)
	assert.False(t, applied)
	assert.Equal(t, "Alice", val)
}

func TestRedactValue_NilRedactor(t *testing.T) {
	t.Parallel()

	var r *Redactor

	val, drop, applied := r.redactValue("", "password", "secret")
	assert.False(t, drop)
	assert.False(t, applied)
	assert.Equal(t, "secret", val)
}

// ===========================================================================
// 5. hashString
// ===========================================================================

func TestHashString_Deterministic(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	h1 := r.hashString("hello")
	h2 := r.hashString("hello")
	assert.Equal(t, h1, h2)
}

func TestHashString_DifferentInputs(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	h1 := r.hashString("foo")
	h2 := r.hashString("bar")
	assert.NotEqual(t, h1, h2)
}

func TestHashString_Empty(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	h := r.hashString("")
	assert.NotEmpty(t, h, "hash of empty string should produce a non-empty output")
	assert.True(t, strings.HasPrefix(h, "sha256:"), "hash should have sha256: prefix")
}

func TestHashString_DifferentRedactorsProduceDifferentHashes(t *testing.T) {
	t.Parallel()

	r1 := mustRedactor(t, nil, "")
	r2 := mustRedactor(t, nil, "")

	h1 := r1.hashString("same-input")
	h2 := r2.hashString("same-input")
	assert.NotEqual(t, h1, h2, "different Redactors use different HMAC keys and should produce different hashes")
}

// ===========================================================================
// 6. obfuscateStructFields -- flat maps
// ===========================================================================

func TestObfuscateStructFields_FlatMap(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
		{FieldPattern: `(?i)^email$`, Action: RedactionHash},
	}, "***")

	input := map[string]any{
		"name":     "alice",
		"email":    "alice@example.com",
		"password": "secret",
	}

	result := obfuscateStructFields(input, "", r)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", m["name"])
	assert.Equal(t, "***", m["password"])
	assert.Equal(t, hashVia(r, "alice@example.com"), m["email"])
}

func TestObfuscateStructFields_EmptyMap(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `^password$`, Action: RedactionMask},
	}, "***")

	result := obfuscateStructFields(map[string]any{}, "", r)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Empty(t, m)
}

func TestObfuscateStructFields_NilRedactor(t *testing.T) {
	t.Parallel()

	input := map[string]any{
		"password": "secret",
	}

	result := obfuscateStructFields(input, "", nil)
	m := result.(map[string]any)
	assert.Equal(t, "secret", m["password"], "nil redactor should pass values through")
}

// ===========================================================================
// 7. obfuscateStructFields -- nested maps
// ===========================================================================

func TestObfuscateStructFields_NestedTwoLevels(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	input := map[string]any{
		"user": map[string]any{
			"name":     "bob",
			"password": "topsecret",
		},
	}

	result := obfuscateStructFields(input, "", r).(map[string]any)
	user := result["user"].(map[string]any)
	assert.Equal(t, "bob", user["name"])
	assert.Equal(t, "***", user["password"])
}

func TestObfuscateStructFields_NestedThreeLevels(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^secret$`, Action: RedactionDrop},
	}, "")

	input := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"secret":  "deep-value",
				"visible": "ok",
			},
		},
	}

	result := obfuscateStructFields(input, "", r).(map[string]any)
	l2 := result["level1"].(map[string]any)["level2"].(map[string]any)
	assert.NotContains(t, l2, "secret")
	assert.Equal(t, "ok", l2["visible"])
}

func TestObfuscateStructFields_NestedPathPattern(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{PathPattern: `^config\.database\.password$`, FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "HIDDEN")

	input := map[string]any{
		"config": map[string]any{
			"database": map[string]any{
				"password": "pg_pass",
				"host":     "localhost",
			},
		},
		"password": "top-level-pass",
	}

	result := obfuscateStructFields(input, "", r).(map[string]any)

	dbCfg := result["config"].(map[string]any)["database"].(map[string]any)
	assert.Equal(t, "HIDDEN", dbCfg["password"])
	assert.Equal(t, "localhost", dbCfg["host"])

	assert.NotEqual(t, "HIDDEN", result["password"], "top-level password should NOT match path-scoped rule")
}

// ===========================================================================
// 8. obfuscateStructFields -- arrays
// ===========================================================================

func TestObfuscateStructFields_ArrayOfObjects(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^token$`, Action: RedactionDrop},
	}, "")

	input := []any{
		map[string]any{"id": "1", "token": "tok_a"},
		map[string]any{"id": "2", "token": "tok_b"},
	}

	result := obfuscateStructFields(input, "", r).([]any)
	require.Len(t, result, 2)

	for i, item := range result {
		m := item.(map[string]any)
		assert.Equal(t, strconv.Itoa(i+1), m["id"])
		assert.NotContains(t, m, "token")
	}
}

func TestObfuscateStructFields_NestedArray(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	input := map[string]any{
		"users": []any{
			map[string]any{"name": "alice", "password": "s1"},
			map[string]any{"name": "bob", "password": "s2"},
		},
	}

	result := obfuscateStructFields(input, "", r).(map[string]any)
	users := result["users"].([]any)
	require.Len(t, users, 2)

	assert.Equal(t, "***", users[0].(map[string]any)["password"])
	assert.Equal(t, "***", users[1].(map[string]any)["password"])
	assert.Equal(t, "alice", users[0].(map[string]any)["name"])
	assert.Equal(t, "bob", users[1].(map[string]any)["name"])
}

func TestObfuscateStructFields_EmptyArray(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	result := obfuscateStructFields([]any{}, "", r).([]any)
	assert.Empty(t, result)
}

// ===========================================================================
// 9. obfuscateStructFields -- mixed types
// ===========================================================================

func TestObfuscateStructFields_MixedTypes(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^secret$`, Action: RedactionMask},
	}, "***")

	input := map[string]any{
		"count":   float64(42),
		"active":  true,
		"secret":  "classified",
		"nothing": nil,
		"name":    "test",
	}

	result := obfuscateStructFields(input, "", r).(map[string]any)
	assert.Equal(t, float64(42), result["count"])
	assert.Equal(t, true, result["active"])
	assert.Equal(t, "***", result["secret"])
	assert.Nil(t, result["nothing"])
	assert.Equal(t, "test", result["name"])
}

func TestObfuscateStructFields_NilValue(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	input := map[string]any{
		"password": nil,
	}

	result := obfuscateStructFields(input, "", r).(map[string]any)
	assert.Equal(t, "***", result["password"])
}

func TestObfuscateStructFields_EmptyStringValue(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	input := map[string]any{
		"password": "",
	}

	result := obfuscateStructFields(input, "", r).(map[string]any)
	assert.Equal(t, "***", result["password"])
}

func TestObfuscateStructFields_NonMapNonArray(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	assert.Equal(t, "hello", obfuscateStructFields("hello", "", r))
	assert.Equal(t, float64(42), obfuscateStructFields(float64(42), "", r))
	assert.Equal(t, true, obfuscateStructFields(true, "", r))
	assert.Nil(t, obfuscateStructFields(nil, "", r))
}

// ===========================================================================
// 10. ObfuscateStruct (public API)
// ===========================================================================

func TestObfuscateStruct_NilInput(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	result, err := ObfuscateStruct(nil, r)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestObfuscateStruct_NilRedactor(t *testing.T) {
	t.Parallel()

	input := map[string]any{"password": "secret"}

	result, err := ObfuscateStruct(input, nil)
	require.NoError(t, err)
	assert.Equal(t, input, result, "nil redactor returns input unchanged")
}

func TestObfuscateStruct_BothNil(t *testing.T) {
	t.Parallel()

	result, err := ObfuscateStruct(nil, nil)
	require.NoError(t, err)
	assert.Nil(t, result)
}

func TestObfuscateStruct_FlatMap(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	input := map[string]any{
		"user":     "alice",
		"password": "s3cr3t",
	}

	result, err := ObfuscateStruct(input, r)
	require.NoError(t, err)

	m := result.(map[string]any)
	assert.Equal(t, "alice", m["user"])
	assert.Equal(t, "***", m["password"])
}

func TestObfuscateStruct_UnmarshalableInput(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	_, err := ObfuscateStruct(make(chan int), r)
	require.Error(t, err)
}

// ===========================================================================
// 11. JSON round-trip tests
// ===========================================================================

func TestJSONRoundTrip_SimplePayload(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
		{FieldPattern: `(?i)^email$`, Action: RedactionHash},
	}, "***")

	jsonInput := `{"name":"alice","email":"alice@b.com","password":"pass"}`

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(jsonInput), &parsed))

	result, err := ObfuscateStruct(parsed, r)
	require.NoError(t, err)

	b, err := json.Marshal(result)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(b, &decoded))

	assert.Equal(t, "alice", decoded["name"])
	assert.Equal(t, "***", decoded["password"])
	assert.Contains(t, decoded["email"].(string), "sha256:")
}

func TestJSONRoundTrip_EmptyJSON(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, nil, "")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{}`), &parsed))

	result, err := ObfuscateStruct(parsed, r)
	require.NoError(t, err)

	b, err := json.Marshal(result)
	require.NoError(t, err)
	assert.Equal(t, "{}", string(b))
}

func TestJSONRoundTrip_LargePayload(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	payload := make(map[string]any, 200)
	for i := range 200 {
		key := fmt.Sprintf("field_%d", i)
		payload[key] = fmt.Sprintf("value_%d", i)
	}
	payload["password"] = "should_be_masked"

	result, err := ObfuscateStruct(payload, r)
	require.NoError(t, err)

	m := result.(map[string]any)
	assert.Equal(t, "***", m["password"])
	assert.Equal(t, "value_0", m["field_0"])
	assert.Equal(t, "value_199", m["field_199"])
}

// ===========================================================================
// 12. All three actions end-to-end through ObfuscateStruct
// ===========================================================================

func TestObfuscateStruct_AllActionsEndToEnd(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
		{FieldPattern: `(?i)^document$`, Action: RedactionHash},
		{FieldPattern: `(?i)^token$`, PathPattern: `^session\.token$`, Action: RedactionDrop},
	}, "***")

	input := map[string]any{
		"password": "secret",
		"document": "123456789",
		"session":  map[string]any{"token": "tok_abc"},
		"name":     "visible",
	}

	result, err := ObfuscateStruct(input, r)
	require.NoError(t, err)

	m := result.(map[string]any)

	assert.Equal(t, "***", m["password"])

	hashed, ok := m["document"].(string)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(hashed, "sha256:"))
	assert.NotEqual(t, "123456789", hashed)

	session, ok := m["session"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, session, "token")

	assert.Equal(t, "visible", m["name"])
}

// ===========================================================================
// 13. Edge cases
// ===========================================================================

func TestObfuscateStruct_NumericValues(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^pin$`, Action: RedactionMask},
	}, "***")

	input := map[string]any{
		"pin":   float64(1234),
		"count": float64(10),
	}

	result, err := ObfuscateStruct(input, r)
	require.NoError(t, err)

	m := result.(map[string]any)
	assert.Equal(t, "***", m["pin"])
	assert.Equal(t, json.Number("10"), m["count"])
}

// ===========================================================================
// 14. Processor: redactAttributesByKey
// ===========================================================================

func TestRedactAttributesByKey_NilRedactor(t *testing.T) {
	t.Parallel()

	attrs := []attribute.KeyValue{
		attribute.String("foo", "bar"),
	}

	result := redactAttributesByKey(attrs, nil)
	assert.Equal(t, attrs, result, "nil redactor returns attributes unchanged")
}

func TestRedactAttributesByKey_EmptyAttrs(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `^password$`, Action: RedactionMask},
	}, "***")

	result := redactAttributesByKey(nil, r)
	assert.Empty(t, result)
}

func TestRedactAttributesByKey_DottedKeyExtractsFieldName(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	attrs := []attribute.KeyValue{
		attribute.String("user.password", "secret"),
		attribute.String("user.name", "alice"),
	}

	result := redactAttributesByKey(attrs, r)
	values := make(map[string]string, len(result))
	for _, a := range result {
		values[string(a.Key)] = a.Value.AsString()
	}

	assert.Equal(t, "***", values["user.password"])
	assert.Equal(t, "alice", values["user.name"])
}

func TestRedactAttributesByKey_DropRemovesAttribute(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^token$`, Action: RedactionDrop},
	}, "")

	attrs := []attribute.KeyValue{
		attribute.String("auth.token", "tok_123"),
		attribute.String("auth.type", "bearer"),
	}

	result := redactAttributesByKey(attrs, r)
	require.Len(t, result, 1)
	assert.Equal(t, "auth.type", string(result[0].Key))
}

func TestRedactAttributesByKey_HashProducesConsistentOutput(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^document$`, Action: RedactionHash},
	}, "")

	attrs := []attribute.KeyValue{
		attribute.String("customer.document", "123456789"),
	}

	result1 := redactAttributesByKey(attrs, r)
	result2 := redactAttributesByKey(attrs, r)

	require.Len(t, result1, 1)
	require.Len(t, result2, 1)
	assert.Equal(t, result1[0].Value.AsString(), result2[0].Value.AsString())
	assert.True(t, strings.HasPrefix(result1[0].Value.AsString(), "sha256:"))
}

func TestRedactAttributesByKey_KeyWithoutDot(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	attrs := []attribute.KeyValue{
		attribute.String("password", "secret"),
	}

	result := redactAttributesByKey(attrs, r)
	require.Len(t, result, 1)
	assert.Equal(t, "***", result[0].Value.AsString())
}

// ===========================================================================
// 15. Processor interface compliance
// ===========================================================================

func TestAttrBagSpanProcessor_NoOpMethods(t *testing.T) {
	t.Parallel()

	p := AttrBagSpanProcessor{}
	assert.NoError(t, p.Shutdown(nil))
	assert.NoError(t, p.ForceFlush(nil))
}

func TestRedactingAttrBagSpanProcessor_NoOpMethods(t *testing.T) {
	t.Parallel()

	p := RedactingAttrBagSpanProcessor{}
	assert.NoError(t, p.Shutdown(nil))
	assert.NoError(t, p.ForceFlush(nil))
}

// ===========================================================================
// 16. RedactionAction constants
// ===========================================================================

func TestRedactionActionConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, RedactionAction("mask"), RedactionMask)
	assert.Equal(t, RedactionAction("hash"), RedactionHash)
	assert.Equal(t, RedactionAction("drop"), RedactionDrop)
}

// ===========================================================================
// 17. Concurrency safety
// ===========================================================================

func TestObfuscateStruct_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
		{FieldPattern: `(?i)^email$`, Action: RedactionHash},
		{FieldPattern: `(?i)^token$`, Action: RedactionDrop},
	}, "***")

	done := make(chan struct{}, 50)
	for i := range 50 {
		go func(idx int) {
			defer func() { done <- struct{}{} }()

			payload := map[string]any{
				"id":       fmt.Sprintf("user_%d", idx),
				"password": "secret",
				"email":    fmt.Sprintf("user%d@test.com", idx),
				"token":    "tok_concurrent",
				"data": map[string]any{
					"password": "nested_secret",
				},
			}

			result, err := ObfuscateStruct(payload, r)
			if err != nil {
				t.Errorf("concurrent ObfuscateStruct failed: %v", err)
				return
			}

			m := result.(map[string]any)
			if m["password"] != "***" {
				t.Errorf("expected masked password, got %v", m["password"])
			}
		}(i)
	}

	for range 50 {
		<-done
	}
}

func TestRedactAttributesByKey_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	r := mustRedactor(t, []RedactionRule{
		{FieldPattern: `(?i)^password$`, Action: RedactionMask},
	}, "***")

	attrs := []attribute.KeyValue{
		attribute.String("user.password", "secret"),
		attribute.String("user.name", "alice"),
	}

	done := make(chan struct{}, 50)
	for range 50 {
		go func() {
			defer func() { done <- struct{}{} }()
			result := redactAttributesByKey(attrs, r)
			if len(result) != 2 {
				t.Errorf("expected 2 attributes, got %d", len(result))
			}
		}()
	}

	for range 50 {
		<-done
	}
}
