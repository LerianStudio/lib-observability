// Package redaction provides sensitive field detection for log and span attribute
// redaction. IsSensitiveField checks a field name against a default list of
// credentials, PII, and financial identifiers, with support for camelCase
// normalization and word-boundary matching. Callers may extend the default list
// with additional field names via the variadic extra parameter.
package redaction
