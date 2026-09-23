package redaction

import (
	"maps"
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

// sensitiveFieldTokens is every token that appears in defaultSensitiveFields:
// "api_key" contributes "api" and "key". A folded plural whose singular is not
// one of these cannot match any default field, so it is discarded before it
// costs a second pass over the list.
var sensitiveFieldTokens = sync.OnceValue(func() map[string]bool {
	tokens := make(map[string]bool, len(defaultSensitiveFields)*2)
	for _, field := range defaultSensitiveFields {
		for _, token := range splitTokens(field) {
			tokens[token] = true
		}
	}

	return tokens
})

// splitTokens splits a field name into its alphanumeric tokens.
func splitTokens(name string) []string {
	return strings.FieldsFunc(name, func(r rune) bool {
		return !isAlphanumeric(r)
	})
}

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

// singularizeTokens folds a single trailing "s" off every token whose singular
// could still match something, appends those singulars to tokens, and returns
// the rejoined singular spelling of the whole name. It returns "" when no fold
// applies -- the common case, which then costs no second matching pass.
//
// Only one trailing "s" is folded, so "es" and "ies" plurals ("addresses",
// "cities") are out of scope, and the original tokens are kept alongside the
// folded ones so "address", "status", "class", "bus" and "pass" keep their own
// verdict. A fold is kept only when its singular is a token of some default
// field, which is lossless: a plural can only match a pattern whose own tokens
// include that singular. Callers passing extra field names fold everything,
// since those names are not in the default vocabulary.
func singularizeTokens(tokens []string, foldAll bool) ([]string, string) {
	known := sensitiveFieldTokens()

	var folded []string

	for i, token := range tokens {
		if len(token) < 2 || token[len(token)-1] != 's' {
			continue
		}

		singular := token[:len(token)-1]
		if !foldAll && !known[singular] {
			continue
		}

		if folded == nil {
			folded = slices.Clone(tokens)
		}

		folded[i] = singular
	}

	if folded == nil {
		return tokens, ""
	}

	return append(tokens, folded...), strings.Join(folded, "_")
}

// matchesDefaultFields reports whether any default sensitive field matches one
// of the candidate spellings. Short tokens (like "key", "auth") must match a
// whole token exactly; longer names match on word boundaries.
func matchesDefaultFields(candidates, tokens []string) bool {
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

// matchesExtraFields reports whether any caller-supplied field name appears,
// on a word boundary, in one of the candidate spellings. Each extra name is
// tried both lowercased and in the underscore-joined form the singular
// candidate uses, so "ledgerId" and "user.name" also match "ledgerIds" and
// "user.names".
func matchesExtraFields(candidates, extra []string) bool {
	for _, e := range extra {
		eLower := strings.ToLower(e)
		eCanonical := strings.Join(splitTokens(normalizeFieldName(e)), "_")

		for _, candidate := range candidates {
			if matchesWordBoundary(candidate, eLower) ||
				(eCanonical != eLower && matchesWordBoundary(candidate, eCanonical)) {
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
// (like "key", "auth") use exact token matching to avoid false positives,
// while longer patterns use word-boundary matching.
//
// Extra fields are additional field names to treat as sensitive beyond the
// built-in default list. Pass them as individual string arguments.
func IsSensitiveField(fieldName string, extra ...string) bool {
	m := ensureSensitiveFieldsMap()
	lowerField := strings.ToLower(fieldName)

	// Hot path: a field literally named "password" or "api_key" is answered
	// here, before anything is normalized, split or allocated.
	if m[lowerField] {
		return true
	}

	for _, e := range extra {
		if strings.EqualFold(fieldName, e) {
			return true
		}
	}

	// A name with no uppercase already is its own normalization.
	normalized := lowerField
	if lowerField != fieldName {
		normalized = normalizeFieldName(fieldName)
		if m[normalized] {
			return true
		}
	}

	tokens, singular := singularizeTokens(splitTokens(normalized), len(extra) > 0)
	if singular != "" && m[singular] {
		return true
	}

	var buf [3]string

	candidates := append(buf[:0], normalized)
	if lowerField != normalized {
		candidates = append(candidates, lowerField)
	}

	if singular != "" {
		candidates = append(candidates, singular)
	}

	return matchesDefaultFields(candidates, tokens) || matchesExtraFields(candidates, extra)
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

		startOk := start == 0 || !isAlphanumeric(rune(field[start-1]))
		endOk := end == len(field) || !isAlphanumeric(rune(field[end]))

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

func isAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}
