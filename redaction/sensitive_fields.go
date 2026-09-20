package redaction

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
)

var defaultSensitiveFields = []string{
	"password",
	"newpassword",
	"oldpassword",
	"passwordsalt",
	"token",
	"secret",
	"key",
	"authorization",
	"auth",
	"credential",
	"credentials",
	"apikey",
	"api_key",
	"access_token",
	"accesstoken",
	"refresh_token",
	"refreshtoken",
	"bearer",
	"jwt",
	"session_id",
	"sessionid",
	"cookie",
	"private_key",
	"privatekey",
	"clientid",
	"client_id",
	"clientsecret",
	"client_secret",
	"passwd",
	"passphrase",
	"pass",
	"pwd",
	"card_number",
	"cardnumber",
	"cvv",
	"cvc",
	"ssn",
	"social_security",
	"pin",
	"otp",
	"account_number",
	"accountnumber",
	"routing_number",
	"routingnumber",
	"iban",
	"swift",
	"swift_code",
	"bic",
	"pan",
	"expiry",
	"expiry_date",
	"expiration_date",
	"card_expiry",
	"date_of_birth",
	"dob",
	"tax_id",
	"taxid",
	"tin",
	"national_id",
	"sort_code",
	"bsb",
	"security_answer",
	"security_question",
	"mother_maiden_name",
	"mfa_code",
	"totp",
	"biometric",
	"fingerprint",
	"certificate",
	"connection_string",
	"database_url",
	// PII fields
	"email",
	"phone",
	"phone_number",
	"address",
	"street",
	"city",
	"zip",
	"postal_code",
}

var (
	sensitiveFieldsMapOnce sync.Once
	sensitiveFieldsMap     map[string]bool
)

// DefaultSensitiveFields returns a copy of the default sensitive field names.
// The returned slice is a clone — callers cannot mutate shared state.
func DefaultSensitiveFields() []string {
	clone := make([]string, len(defaultSensitiveFields))
	copy(clone, defaultSensitiveFields)

	return clone
}

// ensureSensitiveFieldsMap returns the internal map directly (no clone).
// For internal use only where we just need read access.
func ensureSensitiveFieldsMap() map[string]bool {
	sensitiveFieldsMapOnce.Do(func() {
		sensitiveFieldsMap = make(map[string]bool, len(defaultSensitiveFields))
		for _, field := range defaultSensitiveFields {
			sensitiveFieldsMap[field] = true
		}
	})

	return sensitiveFieldsMap
}

// DefaultSensitiveFieldsMap provides a map version of DefaultSensitiveFields
// for lookup operations. All field names are lowercase for
// case-insensitive matching. The underlying cache is initialized only once;
// each call returns a shallow clone so callers cannot mutate shared state.
func DefaultSensitiveFieldsMap() map[string]bool {
	m := ensureSensitiveFieldsMap()
	clone := make(map[string]bool, len(m))
	maps.Copy(clone, m)

	return clone
}

// shortSensitiveTokens contains tokens that are too short or generic for
// substring matching and require exact token matching instead.
var shortSensitiveTokens = map[string]bool{
	"key":  true,
	"auth": true,
	"pass": true,
	"pwd":  true,
	"pin":  true,
	"otp":  true,
	"cvv":  true,
	"cvc":  true,
	"ssn":  true,
	"pan":  true,
	"bic":  true,
	"bsb":  true,
	"dob":  true,
	"tin":  true,
	"jwt":  true,
	"zip":  true,
	"city": true,
}

// tokenSplitRegex splits field names by non-alphanumeric characters.
var tokenSplitRegex = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// normalizeFieldName converts camelCase and PascalCase field names into
// underscore-delimited lowercase tokens. For example, "sessionToken" becomes
// "session_token" and "APIKey" becomes "api_key". This ensures that sensitive
// field detection works for camelCase naming conventions.
func normalizeFieldName(fieldName string) string {
	var b strings.Builder

	runes := []rune(fieldName)

	for i, r := range runes {
		if i > 0 {
			prev := runes[i-1]

			var next rune
			if i+1 < len(runes) {
				next = runes[i+1]
			}

			if unicode.IsUpper(r) &&
				(unicode.IsLower(prev) || unicode.IsDigit(prev) ||
					(unicode.IsUpper(prev) && next != 0 && unicode.IsLower(next))) {
				b.WriteByte('_')
			}
		}

		b.WriteRune(r)
	}

	return strings.ToLower(b.String())
}

// singularizeTokens folds a single trailing "s" off every token of an
// underscore-delimited field name, so "api_keys" also reads as "api_key" and
// "tokens" as "token". Callers judge the original spelling as well, so words
// that merely end in "s" -- "address", "status", "class", "bus", "pass" --
// keep their own verdict. Only one trailing "s" is folded: "es" and "ies"
// plurals such as "addresses" and "cities" are out of scope.
func singularizeTokens(normalized string) string {
	tokens := tokenSplitRegex.Split(normalized, -1)
	folded := false

	for i, token := range tokens {
		if len(token) > 1 && strings.HasSuffix(token, "s") {
			tokens[i] = token[:len(token)-1]
			folded = true
		}
	}

	if !folded {
		return normalized
	}

	return strings.Join(tokens, "_")
}

// candidateSpellings returns the distinct spellings of a field name that
// sensitive-field matching must consider: the name as written, its camelCase
// normalization ("sessionToken" -> "session_token"), and its de-pluralized
// form ("tokens" -> "token"). Each spelling can only add a match; none of them
// replaces the original.
func candidateSpellings(fieldName string) []string {
	lowerField := strings.ToLower(fieldName)
	candidates := []string{lowerField}

	normalized := normalizeFieldName(fieldName)
	if normalized != lowerField {
		candidates = append(candidates, normalized)
	}

	singular := singularizeTokens(normalized)
	if singular != normalized && singular != lowerField {
		candidates = append(candidates, singular)
	}

	return candidates
}

// matchesDefaultFields reports whether any default sensitive field matches one
// of the candidate spellings. Short tokens (like "key", "pwd") must match a
// whole token exactly; longer names match on word boundaries.
func matchesDefaultFields(candidates []string) bool {
	tokens := make([]string, 0, len(candidates)*2)
	for _, candidate := range candidates {
		tokens = append(tokens, tokenSplitRegex.Split(candidate, -1)...)
	}

	for _, sensitive := range defaultSensitiveFields {
		if shortSensitiveTokens[sensitive] {
			if slices.Contains(tokens, sensitive) {
				return true
			}

			continue
		}

		for _, candidate := range candidates {
			if matchesWordBoundary(candidate, sensitive) {
				return true
			}
		}
	}

	return false
}

// IsSensitiveField checks if a field name is considered sensitive based on
// the default sensitive fields list plus any extra fields provided. The check
// is case-insensitive and handles camelCase field names and plural field names
// by normalizing them to underscore-delimited singular tokens. Short tokens
// (like "key", "auth", "pwd") use exact token matching to avoid false
// positives, while longer patterns use word-boundary matching.
//
// Extra fields are additional field names to treat as sensitive beyond the
// built-in default list. Pass them as individual string arguments.
func IsSensitiveField(fieldName string, extra ...string) bool {
	m := ensureSensitiveFieldsMap()
	candidates := candidateSpellings(fieldName)

	for _, candidate := range candidates {
		if m[candidate] {
			return true
		}
	}

	for _, e := range extra {
		if strings.EqualFold(fieldName, e) {
			return true
		}
	}

	if matchesDefaultFields(candidates) {
		return true
	}

	for _, e := range extra {
		eLower := strings.ToLower(e)
		for _, candidate := range candidates {
			if matchesWordBoundary(candidate, eLower) {
				return true
			}
		}
	}

	return false
}

// matchesWordBoundary checks if the pattern appears in the field with word boundaries.
// A word boundary is either the start/end of string or a non-alphanumeric character.
func matchesWordBoundary(field, pattern string) bool {
	if len(pattern) == 0 {
		return false
	}

	idx := strings.Index(field, pattern)
	if idx == -1 {
		return false
	}

	for idx != -1 {
		start := idx
		end := idx + len(pattern)

		startOk := start == 0 || !isAlphanumeric(field[start-1])
		endOk := end == len(field) || !isAlphanumeric(field[end])

		if startOk && endOk {
			return true
		}

		if end >= len(field) {
			break
		}

		nextIdx := strings.Index(field[end:], pattern)
		if nextIdx == -1 {
			break
		}

		idx = end + nextIdx
	}

	return false
}

func isAlphanumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
