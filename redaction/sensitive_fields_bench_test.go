//go:build unit

package redaction

import "testing"

var benchResult bool

// BenchmarkIsSensitiveField covers the three shapes that matter in production:
// an exact hit (the name is literally in the list), a miss on an ordinary
// business field (the overwhelming majority of logged fields), and a hit that
// only the camelCase or plural folding can find.
func BenchmarkIsSensitiveField(b *testing.B) {
	fields := []string{
		"password",
		"api_key",
		"ledger_account_alias",
		"ledgerAccountAlias",
		"sessionToken",
		"api_keys",
		"transaction_operation_balance_scale",
	}

	for _, field := range fields {
		b.Run(field, func(b *testing.B) {
			b.ReportAllocs()

			for b.Loop() {
				benchResult = IsSensitiveField(field)
			}
		})
	}
}
