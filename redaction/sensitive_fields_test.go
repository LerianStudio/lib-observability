//go:build unit

package redaction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultSensitiveFieldsReturnsCopy(t *testing.T) {
	t.Parallel()

	fields := DefaultSensitiveFields()
	require.NotEmpty(t, fields)
	fields[0] = "mutated"

	assert.NotEqual(t, "mutated", DefaultSensitiveFields()[0])
}

func TestDefaultSensitiveFieldsMapReturnsCopy(t *testing.T) {
	t.Parallel()

	fields := DefaultSensitiveFieldsMap()
	require.True(t, fields["password"])
	fields["password"] = false
	fields["custom"] = true

	next := DefaultSensitiveFieldsMap()
	assert.True(t, next["password"])
	assert.False(t, next["custom"])
}

func TestIsSensitiveField(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		field string
		extra []string
		want  bool
	}{
		{name: "exact default", field: "password", want: true},
		{name: "case insensitive", field: "PassWord", want: true},
		{name: "camel case normalized", field: "sessionToken", want: true},
		{name: "acronym camel case normalized", field: "APIKey", want: true},
		{name: "short token word boundary", field: "api_key_hash", want: true},
		{name: "short token avoids substring", field: "monkey", want: false},
		{name: "long token word boundary", field: "customer_email_hash", want: true},
		{name: "extra exact", field: "tenantSecret", extra: []string{"tenantSecret"}, want: true},
		{name: "extra word boundary", field: "billing_custom_field", extra: []string{"custom"}, want: true},
		{name: "safe field", field: "publicIdentifier", want: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsSensitiveField(tt.field, tt.extra...))
		})
	}
}

func TestMatchesWordBoundary(t *testing.T) {
	t.Parallel()

	assert.False(t, matchesWordBoundary("anything", ""))
	assert.False(t, matchesWordBoundary("customeremail", "email"))
	assert.True(t, matchesWordBoundary("customer_email", "email"))
	assert.True(t, matchesWordBoundary("email_address", "email"))
	assert.True(t, matchesWordBoundary("customer-email-value", "email"))
	assert.False(t, isAlphanumeric('-'))
	assert.True(t, isAlphanumeric('a'))
	assert.True(t, isAlphanumeric('Z'))
	assert.True(t, isAlphanumeric('9'))
}

func TestIsSensitiveFieldPassAndPwdTokens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		field string
		want  bool
	}{
		// Credential spellings that products emit as container env vars and log keys.
		{name: "screaming snake pass", field: "DB_PASS", want: true},
		{name: "screaming snake pwd", field: "MYSQL_PWD", want: true},
		{name: "bare pass", field: "pass", want: true},
		{name: "bare pwd", field: "pwd", want: true},
		{name: "upper bare pwd", field: "PWD", want: true},
		{name: "snake pass", field: "db_pass", want: true},
		{name: "snake pwd", field: "redis_pwd", want: true},
		{name: "camel pass", field: "dbPass", want: true},
		{name: "camel pwd", field: "dbPwd", want: true},
		{name: "dotted pass", field: "service.pass", want: true},

		// Ordinary words that merely contain "pass" must not be redacted.
		// "pass" and "pwd" are exact-token matches, not substrings.
		{name: "passenger is not a credential", field: "passenger_count", want: false},
		{name: "compass is not a credential", field: "compass_heading", want: false},
		{name: "bypass is not a credential", field: "bypass_cache", want: false},
		{name: "passes is not a credential", field: "passes", want: false},
		{name: "passive is not a credential", field: "passive_mode", want: false},
		{name: "surpassed is not a credential", field: "surpassed", want: false},

		// Known, accepted over-match: "pwd" as a token also reads a working
		// directory as a credential. Masking a path beats leaking a password.
		{name: "accepted over-match on working directory", field: "pwd_dir", want: true},

		// "passport_number" is deliberately absent: it is PII that arguably
		// belongs in the list, so pinning it to false would cement a leak.
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsSensitiveField(tt.field))
		})
	}
}

func TestIsSensitiveFieldPluralFolding(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		field string
		want  bool
	}{
		// A plural credential field is still a credential field.
		{name: "tokens", field: "tokens", want: true},
		{name: "secrets", field: "secrets", want: true},
		{name: "passwords", field: "passwords", want: true},
		{name: "api keys", field: "api_keys", want: true},
		{name: "db passwords", field: "db_passwords", want: true},
		{name: "camel session tokens", field: "sessionTokens", want: true},
		{name: "short token keys", field: "keys", want: true},
		{name: "short token pins", field: "pins", want: true},
		{name: "short token zips", field: "zips", want: true},

		// The original token keeps its own verdict: folding only ever adds a
		// second spelling to judge, it never replaces the first.
		{name: "address stays sensitive", field: "address", want: true},
		{name: "ssn stays sensitive", field: "ssn", want: true},
		{name: "pass stays sensitive", field: "pass", want: true},

		// Words ending in "s" that are not plurals of a credential.
		{name: "status", field: "status", want: false},
		{name: "class", field: "class", want: false},
		{name: "bus", field: "bus", want: false},
		{name: "passes", field: "passes", want: false},

		// "ies" -> "y" plurals are out of scope: only one trailing "s" is folded.
		{name: "cities stays out", field: "cities", want: false},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, IsSensitiveField(tt.field))
		})
	}
}
