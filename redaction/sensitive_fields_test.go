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
